package grpc

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	merchantv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/merchant/v1"
	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
)

// MerchantEndpointSource 透過 merchant-service 的 ListWebhookEndpoints(include_secrets=true) 取得端點，
// 並以 UpdateWebhookEndpoint(status=DISABLED) 回報端點停用。外層請包 app.EndpointCache。
type MerchantEndpointSource struct {
	client merchantv1.MerchantServiceClient
	clock  app.Clock
}

// NewMerchantEndpointSource 建立來源（conn 由 pkg/grpcx.Dial 建立）。
func NewMerchantEndpointSource(conn grpc.ClientConnInterface, clock app.Clock) *MerchantEndpointSource {
	if clock == nil {
		clock = app.ClockFunc(time.Now)
	}
	return &MerchantEndpointSource{client: merchantv1.NewMerchantServiceClient(conn), clock: clock}
}

// ListEndpoints 實作 app.EndpointSource（逐頁取完）。
func (m *MerchantEndpointSource) ListEndpoints(ctx context.Context, merchantID uuid.UUID) ([]*domain.Endpoint, error) {
	var out []*domain.Endpoint
	token := ""
	for page := 0; page < 50; page++ {
		resp, err := m.client.ListWebhookEndpoints(ctx, &merchantv1.ListWebhookEndpointsRequest{
			MerchantId:     domain.MerchantPublicID(merchantID),
			Page:           &commonv1.PageRequest{PageSize: 100, PageToken: token},
			IncludeSecrets: true,
			IncludeDeleted: false,
		})
		if err != nil {
			return nil, fmt.Errorf("merchant-service ListWebhookEndpoints: %w", err)
		}
		for _, pe := range resp.GetEndpoints() {
			ep, err := m.toDomain(pe, merchantID)
			if err != nil {
				// 單一端點資料異常不應讓整個商戶停擺：略過。
				continue
			}
			out = append(out, ep)
		}
		token = resp.GetPage().GetNextPageToken()
		if token == "" || !resp.GetPage().GetHasMore() {
			break
		}
	}
	return out, nil
}

// GetEndpoint 實作 app.EndpointSource。
func (m *MerchantEndpointSource) GetEndpoint(ctx context.Context, merchantID, endpointID uuid.UUID) (*domain.Endpoint, error) {
	eps, err := m.ListEndpoints(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	for _, ep := range eps {
		if ep.ID == endpointID {
			return ep, nil
		}
	}
	return nil, nil
}

// DisableEndpoint 實作 app.EndpointDisabler（410 Gone → status=DISABLED）。
func (m *MerchantEndpointSource) DisableEndpoint(ctx context.Context, merchantID, endpointID uuid.UUID, reason string) error {
	_, err := m.client.UpdateWebhookEndpoint(ctx, &merchantv1.UpdateWebhookEndpointRequest{
		MerchantId: domain.MerchantPublicID(merchantID),
		Endpoint: &merchantv1.WebhookEndpoint{
			Id:     domain.EndpointPublicID(endpointID),
			Status: merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DISABLED,
			// 把原因留在 metadata 供商戶後台顯示。
			Metadata: map[string]string{"disabled_reason": reason},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status"}},
	})
	if err != nil {
		return fmt.Errorf("merchant-service UpdateWebhookEndpoint: %w", err)
	}
	return nil
}

func (m *MerchantEndpointSource) toDomain(pe *merchantv1.WebhookEndpoint, merchantID uuid.UUID) (*domain.Endpoint, error) {
	id, err := domain.ParseEndpointID(pe.GetId())
	if err != nil {
		return nil, err
	}
	ep := &domain.Endpoint{
		ID:            id,
		MerchantID:    merchantID,
		URL:           pe.GetUrl(),
		EnabledEvents: pe.GetEnabledEvents(),
		Status:        endpointStatus(pe.GetStatus()),
		Livemode:      pe.GetMode() != merchantv1.ApiKeyMode_API_KEY_MODE_TEST,
	}
	ep.Secrets = append(ep.Secrets, pe.GetSigningSecret())
	if prev := pe.GetPreviousSigningSecret(); prev != "" {
		exp := pe.GetPreviousSecretExpiresAt()
		if !exp.IsValid() || exp.AsTime().After(m.clock.Now()) {
			ep.Secrets = append(ep.Secrets, prev)
		}
	}
	return ep, nil
}

func endpointStatus(s merchantv1.WebhookEndpointStatus) domain.EndpointStatus {
	switch s {
	case merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_UNSPECIFIED:
		// 未指定視同停用（不投遞），與 default 一致。
		return domain.EndpointDisabled
	case merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_ENABLED:
		return domain.EndpointEnabled
	case merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DISABLED:
		return domain.EndpointDisabled
	case merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_AUTO_DISABLED:
		return domain.EndpointAutoDisabled
	case merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DELETED:
		return domain.EndpointDeleted
	default:
		return domain.EndpointDisabled
	}
}

var (
	_ app.EndpointSource   = (*MerchantEndpointSource)(nil)
	_ app.EndpointDisabler = (*MerchantEndpointSource)(nil)
)

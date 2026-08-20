package grpc

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	merchantv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/merchant/v1"
	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tsPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func timePtr(t *timestamppb.Timestamp) *time.Time {
	if t == nil {
		return nil
	}
	v := t.AsTime()
	return &v
}

// ---- enums ----

func statusToProto(s domain.Status) merchantv1.MerchantStatus {
	switch s {
	case domain.StatusActive:
		return merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE
	case domain.StatusSuspended:
		return merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED
	case domain.StatusClosed:
		return merchantv1.MerchantStatus_MERCHANT_STATUS_CLOSED
	case domain.StatusPending:
		return merchantv1.MerchantStatus_MERCHANT_STATUS_UNSPECIFIED
	default:
		return merchantv1.MerchantStatus_MERCHANT_STATUS_UNSPECIFIED
	}
}

func statusFromProto(s merchantv1.MerchantStatus) domain.Status {
	switch s {
	case merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE:
		return domain.StatusActive
	case merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED:
		return domain.StatusSuspended
	case merchantv1.MerchantStatus_MERCHANT_STATUS_CLOSED:
		return domain.StatusClosed
	case merchantv1.MerchantStatus_MERCHANT_STATUS_UNSPECIFIED:
		return ""
	default:
		return ""
	}
}

func modeToProto(m domain.Mode) merchantv1.ApiKeyMode {
	switch m {
	case domain.ModeLive:
		return merchantv1.ApiKeyMode_API_KEY_MODE_LIVE
	case domain.ModeTest:
		return merchantv1.ApiKeyMode_API_KEY_MODE_TEST
	default:
		return merchantv1.ApiKeyMode_API_KEY_MODE_UNSPECIFIED
	}
}

// modeFromProto 把 enum 轉成 domain.Mode；UNSPECIFIED 回空字串（由 use case 判斷是否必填）。
func modeFromProto(m merchantv1.ApiKeyMode) domain.Mode {
	switch m {
	case merchantv1.ApiKeyMode_API_KEY_MODE_LIVE:
		return domain.ModeLive
	case merchantv1.ApiKeyMode_API_KEY_MODE_TEST:
		return domain.ModeTest
	case merchantv1.ApiKeyMode_API_KEY_MODE_UNSPECIFIED:
		return ""
	default:
		return domain.Mode("unknown")
	}
}

func keyStatusToProto(s domain.KeyStatus) merchantv1.ApiKeyStatus {
	switch s {
	case domain.KeyActive:
		return merchantv1.ApiKeyStatus_API_KEY_STATUS_ACTIVE
	case domain.KeyRevoked:
		return merchantv1.ApiKeyStatus_API_KEY_STATUS_REVOKED
	case domain.KeyExpired:
		return merchantv1.ApiKeyStatus_API_KEY_STATUS_EXPIRED
	default:
		return merchantv1.ApiKeyStatus_API_KEY_STATUS_UNSPECIFIED
	}
}

func endpointStatusToProto(e *domain.WebhookEndpoint) merchantv1.WebhookEndpointStatus {
	switch {
	case e.IsDeleted():
		return merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DELETED
	case e.AutoDisabled:
		return merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_AUTO_DISABLED
	case e.Status == domain.EndpointDisabled:
		return merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DISABLED
	case e.Status == domain.EndpointEnabled:
		return merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_ENABLED
	default:
		return merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_UNSPECIFIED
	}
}

// endpointStatusFromProto 只接受 ENABLED / DISABLED；其他回傳無效值讓 use case 拒絕。
func endpointStatusFromProto(s merchantv1.WebhookEndpointStatus) domain.EndpointStatus {
	switch s {
	case merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_ENABLED:
		return domain.EndpointEnabled
	case merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DISABLED:
		return domain.EndpointDisabled
	case merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_UNSPECIFIED,
		merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_AUTO_DISABLED,
		merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DELETED:
		return domain.EndpointStatus(s.String())
	default:
		return domain.EndpointStatus(s.String())
	}
}

// ---- messages ----

func merchantToProto(m *domain.Merchant) *merchantv1.Merchant {
	return &merchantv1.Merchant{
		Id:                         m.PublicID(),
		Name:                       m.Name,
		LegalName:                  m.Settings.LegalName,
		Country:                    m.Country,
		DefaultCurrency:            m.DefaultCurrency,
		Status:                     statusToProto(m.Status),
		ContactEmail:               m.Settings.ContactEmail,
		DefaultStatementDescriptor: m.Settings.StatementDescriptor,
		ExternalRef:                m.Settings.ExternalRef,
		Metadata:                   m.Metadata,
		CreatedAt:                  ts(m.CreatedAt),
		UpdatedAt:                  ts(m.UpdatedAt),
	}
}

func apiKeyToProto(k *domain.ApiKey, now time.Time) *merchantv1.ApiKey {
	return &merchantv1.ApiKey{
		Id:         k.PublicID(),
		MerchantId: merchantPublicID(k.MerchantID),
		Mode:       modeToProto(k.Mode),
		Prefix:     k.Prefix,
		Name:       k.Name,
		Scopes:     k.Scopes,
		Status:     keyStatusToProto(k.Status(now)),
		LastUsedAt: tsPtr(k.LastUsedAt),
		ExpiresAt:  tsPtr(k.ExpiresAt),
		CreatedAt:  ts(k.CreatedAt),
		RevokedAt:  tsPtr(k.RevokedAt),
	}
}

func endpointToProto(v app.WebhookEndpointView) *merchantv1.WebhookEndpoint {
	e := v.Endpoint
	out := &merchantv1.WebhookEndpoint{
		Id:                    e.PublicID(),
		MerchantId:            merchantPublicID(e.MerchantID),
		Url:                   e.URL,
		Description:           e.Description,
		EnabledEvents:         e.EnabledEvents,
		Status:                endpointStatusToProto(e),
		Mode:                  modeToProto(e.Mode),
		Metadata:              e.Metadata,
		CreatedAt:             ts(e.CreatedAt),
		UpdatedAt:             ts(e.UpdatedAt),
		SigningSecret:         v.Secret,
		PreviousSigningSecret: v.PreviousSecret,
	}
	if v.PreviousSecret != "" {
		out.PreviousSecretExpiresAt = tsPtr(e.PreviousSecretExpiresAt())
	}
	return out
}

func routingToProto(p *domain.RoutingPreferences) *merchantv1.RoutingPreferences {
	rules := make([]*merchantv1.RoutingRule, 0, len(p.Rules))
	for _, r := range p.Rules {
		rules = append(rules, &merchantv1.RoutingRule{
			Priority: r.Priority, Currencies: r.Currencies, PaymentMethodTypes: r.PaymentMethodTypes, CardBrands: r.CardBrands,
			Countries: r.Countries, Provider: r.Provider, Enabled: r.Enabled,
		})
	}
	return &merchantv1.RoutingPreferences{
		MerchantId:        merchantPublicID(p.MerchantID),
		Rules:             rules,
		FallbackProviders: p.FallbackProviders,
		FailoverEnabled:   p.FailoverEnabled,
		MaxAttempts:       int32(p.MaxAttempts), //nolint:gosec // 1..5
		UpdatedAt:         tsPtr(p.UpdatedAt),
	}
}

func rulesFromProto(in []*merchantv1.RoutingRule) []domain.RoutingRule {
	out := make([]domain.RoutingRule, 0, len(in))
	for _, r := range in {
		if r == nil {
			continue
		}
		out = append(out, domain.RoutingRule{
			Priority: r.GetPriority(), Currencies: r.GetCurrencies(), PaymentMethodTypes: r.GetPaymentMethodTypes(), CardBrands: r.GetCardBrands(),
			Countries: r.GetCountries(), Provider: r.GetProvider(), Enabled: r.GetEnabled(),
		})
	}
	return out
}

func pageFromProto(p *commonv1.PageRequest) app.Page {
	return app.Page{Size: int(p.GetPageSize()), Token: p.GetPageToken()}
}

func pageToProto(next string) *commonv1.PageResponse {
	return &commonv1.PageResponse{NextPageToken: next, HasMore: next != ""}
}

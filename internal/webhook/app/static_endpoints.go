package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
)

// ErrInvalidPageToken 為分頁 token 無效（→ INVALID_ARGUMENT）。
var ErrInvalidPageToken = apperr.ErrParameterInvalid.WithParam("page_token").WithMessage("page_token is invalid.")

// StaticEndpointSource 為開發用端點來源：對所有商戶回傳同一組端點（merchant_id 以查詢者為準）。
// 只在 dev 環境、未設定 PG_MERCHANT_SERVICE_ADDR 時使用，搭配 devsink 做本機投遞測試。
type StaticEndpointSource struct {
	Endpoints []*domain.Endpoint
}

// ListEndpoints 實作 EndpointSource。
func (s *StaticEndpointSource) ListEndpoints(_ context.Context, merchantID uuid.UUID) ([]*domain.Endpoint, error) {
	out := make([]*domain.Endpoint, 0, len(s.Endpoints))
	for _, ep := range s.Endpoints {
		c := *ep
		c.MerchantID = merchantID
		out = append(out, &c)
	}
	return out, nil
}

// GetEndpoint 實作 EndpointSource。
func (s *StaticEndpointSource) GetEndpoint(ctx context.Context, merchantID, endpointID uuid.UUID) (*domain.Endpoint, error) {
	eps, _ := s.ListEndpoints(ctx, merchantID)
	for _, ep := range eps {
		if ep.ID == endpointID {
			return ep, nil
		}
	}
	return nil, nil
}

var _ EndpointSource = (*StaticEndpointSource)(nil)

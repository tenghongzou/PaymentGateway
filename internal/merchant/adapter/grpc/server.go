// Package grpc 為 merchant-service 的 gRPC adapter：實作 pg.merchant.v1.MerchantService 的全部 14 個 rpc，
// 把 proto 轉成 app 的輸入、把領域錯誤以 grpcx.ErrorFromDomain 轉成 gRPC status。
package grpc

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	merchantv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/merchant/v1"
	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// Server 實作 pg.merchant.v1.MerchantService。
type Server struct {
	merchantv1.UnimplementedMerchantServiceServer
	svc   *app.Service
	clock app.Clock
}

// NewServer 建立 Server。
func NewServer(svc *app.Service, clock app.Clock) *Server {
	if clock == nil {
		clock = app.SystemClock{}
	}
	return &Server{svc: svc, clock: clock}
}

// Register 把服務註冊到 gRPC server。
func (s *Server) Register(srv *grpc.Server) {
	merchantv1.RegisterMerchantServiceServer(srv, s)
}

func merchantPublicID(id uuid.UUID) string { return ids.Format(ids.PrefixMerchant, id) }

// ---- Merchant ----

// CreateMerchant 建立商戶。
func (s *Server) CreateMerchant(ctx context.Context, req *merchantv1.CreateMerchantRequest) (*merchantv1.CreateMerchantResponse, error) {
	m, err := s.svc.CreateMerchant(ctx, app.CreateMerchantInput{
		Name: req.GetName(), LegalName: req.GetLegalName(), Country: req.GetCountry(), DefaultCurrency: req.GetDefaultCurrency(),
		ContactEmail: req.GetContactEmail(), StatementDescriptor: req.GetDefaultStatementDescriptor(), ExternalRef: req.GetExternalRef(), Metadata: req.GetMetadata(),
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &merchantv1.CreateMerchantResponse{Merchant: merchantToProto(m)}, nil
}

// GetMerchant 取得商戶。
func (s *Server) GetMerchant(ctx context.Context, req *merchantv1.GetMerchantRequest) (*merchantv1.GetMerchantResponse, error) {
	m, err := s.svc.GetMerchant(ctx, req.GetMerchantId())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &merchantv1.GetMerchantResponse{Merchant: merchantToProto(m)}, nil
}

// UpdateMerchant 部分更新商戶。
func (s *Server) UpdateMerchant(ctx context.Context, req *merchantv1.UpdateMerchantRequest) (*merchantv1.UpdateMerchantResponse, error) {
	pm := req.GetMerchant()
	if pm == nil {
		return nil, grpcx.ErrorFromDomain(domain.ErrParameterMissing.WithParam("merchant"))
	}
	m, err := s.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{
		MerchantID: pm.GetId(),
		Fields:     req.GetUpdateMask().GetPaths(),
		Patch: app.MerchantPatch{
			Name: pm.GetName(), LegalName: pm.GetLegalName(), ContactEmail: pm.GetContactEmail(), Status: statusFromProto(pm.GetStatus()),
			DefaultCurrency: pm.GetDefaultCurrency(), StatementDescriptor: pm.GetDefaultStatementDescriptor(), Metadata: pm.GetMetadata(),
		},
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &merchantv1.UpdateMerchantResponse{Merchant: merchantToProto(m)}, nil
}

// ListMerchants 列出商戶。
func (s *Server) ListMerchants(ctx context.Context, req *merchantv1.ListMerchantsRequest) (*merchantv1.ListMerchantsResponse, error) {
	items, next, err := s.svc.ListMerchants(ctx, app.ListMerchantsInput{
		Status: statusFromProto(req.GetStatus()), Country: req.GetCountry(), Page: pageFromProto(req.GetPage()),
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	out := make([]*merchantv1.Merchant, 0, len(items))
	for _, m := range items {
		out = append(out, merchantToProto(m))
	}
	return &merchantv1.ListMerchantsResponse{Merchants: out, Page: pageToProto(next)}, nil
}

// ---- API key ----

// CreateApiKey 建立 API Key；secret / signing_secret 只在此回應出現一次。
func (s *Server) CreateApiKey(ctx context.Context, req *merchantv1.CreateApiKeyRequest) (*merchantv1.CreateApiKeyResponse, error) { //nolint:revive // 方法名由 proto 產生的介面固定
	out, err := s.svc.CreateApiKey(ctx, app.CreateApiKeyInput{
		MerchantID: req.GetMerchantId(), Mode: modeFromProto(req.GetMode()), Name: req.GetName(), Scopes: req.GetScopes(), ExpiresAt: timePtr(req.GetExpiresAt()),
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &merchantv1.CreateApiKeyResponse{ApiKey: apiKeyToProto(out.Key, s.clock.Now()), Secret: out.Plaintext, SigningSecret: out.SigningSecret}, nil
}

// RevokeApiKey 撤銷 API Key（冪等）。
func (s *Server) RevokeApiKey(ctx context.Context, req *merchantv1.RevokeApiKeyRequest) (*merchantv1.RevokeApiKeyResponse, error) { //nolint:revive // 方法名由 proto 產生的介面固定
	k, err := s.svc.RevokeApiKey(ctx, req.GetMerchantId(), req.GetApiKeyId(), req.GetReason())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &merchantv1.RevokeApiKeyResponse{ApiKey: apiKeyToProto(k, s.clock.Now())}, nil
}

// ListApiKeys 列出 API Key。
func (s *Server) ListApiKeys(ctx context.Context, req *merchantv1.ListApiKeysRequest) (*merchantv1.ListApiKeysResponse, error) { //nolint:revive // 方法名由 proto 產生的介面固定
	items, next, err := s.svc.ListApiKeys(ctx, app.ListApiKeysInput{
		MerchantID: req.GetMerchantId(), Mode: modeFromProto(req.GetMode()), IncludeInactive: req.GetIncludeInactive(), Page: pageFromProto(req.GetPage()),
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	now := s.clock.Now()
	out := make([]*merchantv1.ApiKey, 0, len(items))
	for _, k := range items {
		out = append(out, apiKeyToProto(k, now))
	}
	return &merchantv1.ListApiKeysResponse{ApiKeys: out, Page: pageToProto(next)}, nil
}

// VerifyApiKey 驗證明文 key。無效時回 valid=false + reason（不回 gRPC error）。
//
// 注意：查找用的 prefix 一律由完整 key 推導（前 16 碼 = pk_<mode>_ + 8 碼 lookup_id），
// req.key_prefix 只作為一致性檢查（不一致視為 not_found）。
func (s *Server) VerifyApiKey(ctx context.Context, req *merchantv1.VerifyApiKeyRequest) (*merchantv1.VerifyApiKeyResponse, error) { //nolint:revive // 方法名由 proto 產生的介面固定
	key := strings.TrimSpace(req.GetKey())
	if p := req.GetKeyPrefix(); p != "" && !strings.HasPrefix(key, p) {
		return &merchantv1.VerifyApiKeyResponse{Valid: false, Reason: app.ReasonNotFound}, nil
	}
	res, err := s.svc.VerifyApiKey(ctx, key)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	if !res.Valid {
		return &merchantv1.VerifyApiKeyResponse{Valid: false, Reason: res.Reason}, nil
	}
	return &merchantv1.VerifyApiKeyResponse{
		Valid:                 true,
		MerchantId:            res.Merchant.PublicID(),
		ApiKeyId:              res.Key.PublicID(),
		Mode:                  modeToProto(res.Key.Mode),
		Scopes:                res.Key.Scopes,
		MerchantStatus:        statusToProto(res.Merchant.Status),
		SigningSecret:         res.SigningSecret,
		PreviousSigningSecret: res.PreviousSigningSecret,
	}, nil
}

// ---- Webhook endpoints ----

// CreateWebhookEndpoint 建立端點；signing_secret 僅此一次。
func (s *Server) CreateWebhookEndpoint(ctx context.Context, req *merchantv1.CreateWebhookEndpointRequest) (*merchantv1.CreateWebhookEndpointResponse, error) {
	v, err := s.svc.CreateWebhookEndpoint(ctx, app.CreateWebhookEndpointInput{
		MerchantID: req.GetMerchantId(), URL: req.GetUrl(), Description: req.GetDescription(), EnabledEvents: req.GetEnabledEvents(),
		Mode: modeFromProto(req.GetMode()), Metadata: req.GetMetadata(),
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &merchantv1.CreateWebhookEndpointResponse{Endpoint: endpointToProto(*v)}, nil
}

// UpdateWebhookEndpoint 部分更新 / 輪替 secret。
func (s *Server) UpdateWebhookEndpoint(ctx context.Context, req *merchantv1.UpdateWebhookEndpointRequest) (*merchantv1.UpdateWebhookEndpointResponse, error) {
	pe := req.GetEndpoint()
	if pe == nil {
		return nil, grpcx.ErrorFromDomain(domain.ErrParameterMissing.WithParam("endpoint"))
	}
	v, err := s.svc.UpdateWebhookEndpoint(ctx, app.UpdateWebhookEndpointInput{
		MerchantID: req.GetMerchantId(), EndpointID: pe.GetId(), Fields: req.GetUpdateMask().GetPaths(), RotateSecret: req.GetRotateSecret(),
		Patch: app.WebhookEndpointPatch{
			URL: pe.GetUrl(), Description: pe.GetDescription(), EnabledEvents: pe.GetEnabledEvents(), Status: endpointStatusFromProto(pe.GetStatus()), Metadata: pe.GetMetadata(),
		},
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	// 非輪替時不回傳任何 secret。
	if !req.GetRotateSecret() {
		v.Secret, v.PreviousSecret = "", ""
	}
	return &merchantv1.UpdateWebhookEndpointResponse{Endpoint: endpointToProto(*v)}, nil
}

// DeleteWebhookEndpoint 軟刪除（冪等）。
func (s *Server) DeleteWebhookEndpoint(ctx context.Context, req *merchantv1.DeleteWebhookEndpointRequest) (*merchantv1.DeleteWebhookEndpointResponse, error) {
	if err := s.svc.DeleteWebhookEndpoint(ctx, req.GetMerchantId(), req.GetEndpointId()); err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &merchantv1.DeleteWebhookEndpointResponse{}, nil
}

// ListWebhookEndpoints 列出端點。
func (s *Server) ListWebhookEndpoints(ctx context.Context, req *merchantv1.ListWebhookEndpointsRequest) (*merchantv1.ListWebhookEndpointsResponse, error) {
	views, next, err := s.svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{
		MerchantID: req.GetMerchantId(), Mode: modeFromProto(req.GetMode()), IncludeSecrets: req.GetIncludeSecrets(), IncludeDeleted: req.GetIncludeDeleted(),
		Page: pageFromProto(req.GetPage()),
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	out := make([]*merchantv1.WebhookEndpoint, 0, len(views))
	for _, v := range views {
		out = append(out, endpointToProto(v))
	}
	return &merchantv1.ListWebhookEndpointsResponse{Endpoints: out, Page: pageToProto(next)}, nil
}

// ---- Routing ----

// GetRoutingPreferences 取得路由偏好（未設定回系統預設）。
func (s *Server) GetRoutingPreferences(ctx context.Context, req *merchantv1.GetRoutingPreferencesRequest) (*merchantv1.GetRoutingPreferencesResponse, error) {
	p, err := s.svc.GetRoutingPreferences(ctx, req.GetMerchantId())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &merchantv1.GetRoutingPreferencesResponse{Preferences: routingToProto(p)}, nil
}

// UpdateRoutingPreferences 整份覆寫路由偏好。
func (s *Server) UpdateRoutingPreferences(ctx context.Context, req *merchantv1.UpdateRoutingPreferencesRequest) (*merchantv1.UpdateRoutingPreferencesResponse, error) {
	pp := req.GetPreferences()
	if pp == nil {
		return nil, grpcx.ErrorFromDomain(domain.ErrParameterMissing.WithParam("preferences"))
	}
	p, err := s.svc.UpdateRoutingPreferences(ctx, app.UpdateRoutingPreferencesInput{
		MerchantID: pp.GetMerchantId(), Rules: rulesFromProto(pp.GetRules()), FallbackProviders: pp.GetFallbackProviders(),
		FailoverEnabled: pp.GetFailoverEnabled(), MaxAttempts: int(pp.GetMaxAttempts()),
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &merchantv1.UpdateRoutingPreferencesResponse{Preferences: routingToProto(p)}, nil
}

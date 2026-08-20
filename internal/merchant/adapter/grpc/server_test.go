package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	merchantv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/merchant/v1"
	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/internal/merchant/app/apptest"
	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
)

func newServer(t *testing.T) (*Server, *apptest.Memory, *apptest.Clock) {
	t.Helper()
	mem := apptest.NewMemory()
	clock := apptest.NewClock(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	mem.Clock = clock
	deps := mem.Deps()
	deps.Clock = clock
	deps.Cipher = domain.PlaintextCipher{}
	svc, err := app.New(deps, app.Config{AllowInsecureWebhookURL: true, KnownProviders: []string{"mock", "stripe"}, SyncLastUsed: true})
	require.NoError(t, err)
	return NewServer(svc, clock), mem, clock
}

func createMerchant(t *testing.T, s *Server) *merchantv1.Merchant {
	t.Helper()
	resp, err := s.CreateMerchant(context.Background(), &merchantv1.CreateMerchantRequest{
		Name: "Acme", LegalName: "Acme Ltd", Country: "TW", DefaultCurrency: "TWD", ContactEmail: "ops@acme.example", Metadata: map[string]string{"a": "b"},
	})
	require.NoError(t, err)
	return resp.GetMerchant()
}

func detail(t *testing.T, err error) *commonv1.ErrorDetail {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok)
	d := grpcx.ErrorDetailFromStatus(st)
	require.NotNil(t, d)
	return d
}

func TestMerchantRPCs(t *testing.T) {
	s, mem, _ := newServer(t)
	ctx := context.Background()
	m := createMerchant(t, s)
	assert.Regexp(t, `^mch_`, m.GetId())
	assert.Equal(t, merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE, m.GetStatus())
	assert.Equal(t, "Acme Ltd", m.GetLegalName())
	assert.Equal(t, "ops@acme.example", m.GetContactEmail())
	assert.NotNil(t, m.GetCreatedAt())
	assert.Equal(t, []string{app.EventMerchantCreated}, mem.OutboxTypes())

	got, err := s.GetMerchant(ctx, &merchantv1.GetMerchantRequest{MerchantId: m.GetId()})
	require.NoError(t, err)
	assert.Equal(t, m.GetId(), got.GetMerchant().GetId())

	_, err = s.GetMerchant(ctx, &merchantv1.GetMerchantRequest{MerchantId: "mch_01J5X1Y2Z3A4B5C6D7E8F9G0H1"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = s.GetMerchant(ctx, &merchantv1.GetMerchantRequest{MerchantId: ""})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, "merchant_id", detail(t, err).GetParam())

	_, err = s.CreateMerchant(ctx, &merchantv1.CreateMerchantRequest{Name: "x"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, "parameter_missing", detail(t, err).GetCode())

	upd, err := s.UpdateMerchant(ctx, &merchantv1.UpdateMerchantRequest{
		Merchant:   &merchantv1.Merchant{Id: m.GetId(), Name: "Acme 2", Status: merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name", "status"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "Acme 2", upd.GetMerchant().GetName())
	assert.Equal(t, merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED, upd.GetMerchant().GetStatus())

	_, err = s.UpdateMerchant(ctx, &merchantv1.UpdateMerchantRequest{Merchant: &merchantv1.Merchant{Id: m.GetId()}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"created_at"}}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.UpdateMerchant(ctx, &merchantv1.UpdateMerchantRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	list, err := s.ListMerchants(ctx, &merchantv1.ListMerchantsRequest{Status: merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED})
	require.NoError(t, err)
	assert.Len(t, list.GetMerchants(), 1)
	assert.False(t, list.GetPage().GetHasMore())
	list, err = s.ListMerchants(ctx, &merchantv1.ListMerchantsRequest{Country: "JP"})
	require.NoError(t, err)
	assert.Empty(t, list.GetMerchants())
}

func TestApiKeyRPCs(t *testing.T) {
	s, _, clock := newServer(t)
	ctx := context.Background()
	m := createMerchant(t, s)

	created, err := s.CreateApiKey(ctx, &merchantv1.CreateApiKeyRequest{
		MerchantId: m.GetId(), Mode: merchantv1.ApiKeyMode_API_KEY_MODE_TEST, Name: "backend", Scopes: []string{"payments:write"},
		ExpiresAt: timestamppb.New(clock.Now().Add(time.Hour)),
	})
	require.NoError(t, err)
	assert.Regexp(t, `^pk_test_[0-9A-Za-z]{43}$`, created.GetSecret())
	assert.Regexp(t, `^sk_test_[0-9A-Za-z]{43}$`, created.GetSigningSecret())
	assert.Equal(t, created.GetSecret()[:16], created.GetApiKey().GetPrefix())
	assert.Equal(t, merchantv1.ApiKeyStatus_API_KEY_STATUS_ACTIVE, created.GetApiKey().GetStatus())
	assert.Equal(t, m.GetId(), created.GetApiKey().GetMerchantId())
	assert.NotNil(t, created.GetApiKey().GetExpiresAt())

	_, err = s.CreateApiKey(ctx, &merchantv1.CreateApiKeyRequest{MerchantId: m.GetId(), Name: "no mode"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, "mode", detail(t, err).GetParam())

	// Verify
	v, err := s.VerifyApiKey(ctx, &merchantv1.VerifyApiKeyRequest{KeyPrefix: created.GetSecret()[:12], Key: created.GetSecret()})
	require.NoError(t, err)
	require.True(t, v.GetValid())
	assert.Equal(t, m.GetId(), v.GetMerchantId())
	assert.Equal(t, created.GetApiKey().GetId(), v.GetApiKeyId())
	assert.Equal(t, merchantv1.ApiKeyMode_API_KEY_MODE_TEST, v.GetMode())
	assert.Equal(t, []string{"payments:write"}, v.GetScopes())
	assert.Equal(t, merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE, v.GetMerchantStatus())
	assert.Equal(t, created.GetSigningSecret(), v.GetSigningSecret())
	assert.Empty(t, v.GetPreviousSigningSecret())

	v, err = s.VerifyApiKey(ctx, &merchantv1.VerifyApiKeyRequest{KeyPrefix: "pk_test_zzzz", Key: created.GetSecret()})
	require.NoError(t, err)
	assert.False(t, v.GetValid())
	assert.Equal(t, "not_found", v.GetReason())
	assert.Empty(t, v.GetSigningSecret())

	v, err = s.VerifyApiKey(ctx, &merchantv1.VerifyApiKeyRequest{Key: "garbage"})
	require.NoError(t, err)
	assert.False(t, v.GetValid())

	// expired
	clock.Advance(2 * time.Hour)
	v, err = s.VerifyApiKey(ctx, &merchantv1.VerifyApiKeyRequest{Key: created.GetSecret()})
	require.NoError(t, err)
	assert.False(t, v.GetValid())
	assert.Equal(t, "expired", v.GetReason())

	// list + revoke
	list, err := s.ListApiKeys(ctx, &merchantv1.ListApiKeysRequest{MerchantId: m.GetId(), IncludeInactive: true})
	require.NoError(t, err)
	require.Len(t, list.GetApiKeys(), 1)
	assert.Equal(t, merchantv1.ApiKeyStatus_API_KEY_STATUS_EXPIRED, list.GetApiKeys()[0].GetStatus())
	list, err = s.ListApiKeys(ctx, &merchantv1.ListApiKeysRequest{MerchantId: m.GetId()})
	require.NoError(t, err)
	assert.Empty(t, list.GetApiKeys())

	rev, err := s.RevokeApiKey(ctx, &merchantv1.RevokeApiKeyRequest{MerchantId: m.GetId(), ApiKeyId: created.GetApiKey().GetId(), Reason: "rotate"})
	require.NoError(t, err)
	assert.Equal(t, merchantv1.ApiKeyStatus_API_KEY_STATUS_REVOKED, rev.GetApiKey().GetStatus())
	assert.NotNil(t, rev.GetApiKey().GetRevokedAt())
	v, err = s.VerifyApiKey(ctx, &merchantv1.VerifyApiKeyRequest{Key: created.GetSecret()})
	require.NoError(t, err)
	assert.Equal(t, "revoked", v.GetReason())

	_, err = s.RevokeApiKey(ctx, &merchantv1.RevokeApiKeyRequest{MerchantId: m.GetId(), ApiKeyId: "key_01J5X1Y2Z3A4B5C6D7E8F9G0H1"})
	assert.Equal(t, codes.NotFound, status.Code(err))

	// closed merchant → PermissionDenied on create, merchant_closed on verify
	fresh, err := s.CreateApiKey(ctx, &merchantv1.CreateApiKeyRequest{MerchantId: m.GetId(), Mode: merchantv1.ApiKeyMode_API_KEY_MODE_LIVE, Name: "live"})
	require.NoError(t, err)
	_, err = s.UpdateMerchant(ctx, &merchantv1.UpdateMerchantRequest{Merchant: &merchantv1.Merchant{Id: m.GetId(), Status: merchantv1.MerchantStatus_MERCHANT_STATUS_CLOSED}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status"}}})
	require.NoError(t, err)
	_, err = s.CreateApiKey(ctx, &merchantv1.CreateApiKeyRequest{MerchantId: m.GetId(), Mode: merchantv1.ApiKeyMode_API_KEY_MODE_LIVE, Name: "x"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, "merchant_closed", detail(t, err).GetCode())
	v, err = s.VerifyApiKey(ctx, &merchantv1.VerifyApiKeyRequest{Key: fresh.GetSecret()})
	require.NoError(t, err)
	assert.False(t, v.GetValid())
	assert.Equal(t, "merchant_closed", v.GetReason())
}

func TestWebhookEndpointRPCs(t *testing.T) {
	s, mem, clock := newServer(t)
	ctx := context.Background()
	m := createMerchant(t, s)
	mem.ResetOutbox()

	created, err := s.CreateWebhookEndpoint(ctx, &merchantv1.CreateWebhookEndpointRequest{
		MerchantId: m.GetId(), Url: "http://localhost:3000/hooks", Description: "dev", EnabledEvents: []string{"payment.captured"}, Mode: merchantv1.ApiKeyMode_API_KEY_MODE_TEST,
	})
	require.NoError(t, err)
	ep := created.GetEndpoint()
	assert.Regexp(t, `^we_`, ep.GetId())
	assert.Regexp(t, `^whsec_[0-9A-Za-z]{43}$`, ep.GetSigningSecret())
	assert.Equal(t, merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_ENABLED, ep.GetStatus())
	assert.Equal(t, merchantv1.ApiKeyMode_API_KEY_MODE_TEST, ep.GetMode())
	assert.Equal(t, m.GetId(), ep.GetMerchantId())

	_, err = s.CreateWebhookEndpoint(ctx, &merchantv1.CreateWebhookEndpointRequest{MerchantId: m.GetId(), Url: "ftp://x.example.com/", Mode: merchantv1.ApiKeyMode_API_KEY_MODE_TEST})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, "webhook_url_invalid", detail(t, err).GetCode())
	_, err = s.CreateWebhookEndpoint(ctx, &merchantv1.CreateWebhookEndpointRequest{MerchantId: m.GetId(), Url: "https://x.example.com/", Mode: merchantv1.ApiKeyMode_API_KEY_MODE_TEST, EnabledEvents: []string{"nope"}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// update without rotate: no secrets in response
	upd, err := s.UpdateWebhookEndpoint(ctx, &merchantv1.UpdateWebhookEndpointRequest{
		MerchantId: m.GetId(), Endpoint: &merchantv1.WebhookEndpoint{Id: ep.GetId(), Status: merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DISABLED, Description: "paused"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status", "description"}},
	})
	require.NoError(t, err)
	assert.Equal(t, merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DISABLED, upd.GetEndpoint().GetStatus())
	assert.Equal(t, "paused", upd.GetEndpoint().GetDescription())
	assert.Empty(t, upd.GetEndpoint().GetSigningSecret())

	// status DELETED via update → invalid
	_, err = s.UpdateWebhookEndpoint(ctx, &merchantv1.UpdateWebhookEndpointRequest{
		MerchantId: m.GetId(), Endpoint: &merchantv1.WebhookEndpoint{Id: ep.GetId(), Status: merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DELETED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status"}},
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// rotate
	rot, err := s.UpdateWebhookEndpoint(ctx, &merchantv1.UpdateWebhookEndpointRequest{MerchantId: m.GetId(), Endpoint: &merchantv1.WebhookEndpoint{Id: ep.GetId()}, RotateSecret: true})
	require.NoError(t, err)
	assert.NotEqual(t, ep.GetSigningSecret(), rot.GetEndpoint().GetSigningSecret())
	assert.Equal(t, ep.GetSigningSecret(), rot.GetEndpoint().GetPreviousSigningSecret())
	require.NotNil(t, rot.GetEndpoint().GetPreviousSecretExpiresAt())
	assert.Equal(t, clock.Now().Add(domain.SecretRotationGrace), rot.GetEndpoint().GetPreviousSecretExpiresAt().AsTime())

	// list with secrets
	list, err := s.ListWebhookEndpoints(ctx, &merchantv1.ListWebhookEndpointsRequest{MerchantId: m.GetId(), IncludeSecrets: true})
	require.NoError(t, err)
	require.Len(t, list.GetEndpoints(), 1)
	assert.Equal(t, rot.GetEndpoint().GetSigningSecret(), list.GetEndpoints()[0].GetSigningSecret())
	assert.Equal(t, ep.GetSigningSecret(), list.GetEndpoints()[0].GetPreviousSigningSecret())
	list, err = s.ListWebhookEndpoints(ctx, &merchantv1.ListWebhookEndpointsRequest{MerchantId: m.GetId(), Mode: merchantv1.ApiKeyMode_API_KEY_MODE_LIVE})
	require.NoError(t, err)
	assert.Empty(t, list.GetEndpoints())

	// delete (idempotent) → DELETED in include_deleted list
	_, err = s.DeleteWebhookEndpoint(ctx, &merchantv1.DeleteWebhookEndpointRequest{MerchantId: m.GetId(), EndpointId: ep.GetId()})
	require.NoError(t, err)
	_, err = s.DeleteWebhookEndpoint(ctx, &merchantv1.DeleteWebhookEndpointRequest{MerchantId: m.GetId(), EndpointId: ep.GetId()})
	require.NoError(t, err)
	list, err = s.ListWebhookEndpoints(ctx, &merchantv1.ListWebhookEndpointsRequest{MerchantId: m.GetId(), IncludeDeleted: true})
	require.NoError(t, err)
	require.Len(t, list.GetEndpoints(), 1)
	assert.Equal(t, merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DELETED, list.GetEndpoints()[0].GetStatus())
	_, err = s.DeleteWebhookEndpoint(ctx, &merchantv1.DeleteWebhookEndpointRequest{MerchantId: m.GetId(), EndpointId: "we_01J5X1Y2Z3A4B5C6D7E8F9G0H1"})
	assert.Equal(t, codes.NotFound, status.Code(err))

	assert.Equal(t, []string{
		app.EventWebhookEndpointCreated, app.EventWebhookEndpointUpdated, app.EventWebhookSecretRotated, app.EventWebhookEndpointDeleted,
	}, mem.OutboxTypes())
}

func TestRoutingRPCs(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	m := createMerchant(t, s)

	def, err := s.GetRoutingPreferences(ctx, &merchantv1.GetRoutingPreferencesRequest{MerchantId: m.GetId()})
	require.NoError(t, err)
	assert.Empty(t, def.GetPreferences().GetRules())
	assert.True(t, def.GetPreferences().GetFailoverEnabled())
	assert.Equal(t, int32(3), def.GetPreferences().GetMaxAttempts())
	assert.Nil(t, def.GetPreferences().GetUpdatedAt())

	upd, err := s.UpdateRoutingPreferences(ctx, &merchantv1.UpdateRoutingPreferencesRequest{Preferences: &merchantv1.RoutingPreferences{
		MerchantId: m.GetId(),
		Rules: []*merchantv1.RoutingRule{
			{Priority: 10, Provider: "stripe", Currencies: []string{"USD"}, Enabled: true},
			{Priority: 5, Provider: "mock", PaymentMethodTypes: []string{"card"}, CardBrands: []string{"visa"}, Enabled: true},
		},
		FallbackProviders: []string{"mock"}, FailoverEnabled: true, MaxAttempts: 2,
	}})
	require.NoError(t, err)
	assert.Equal(t, int32(5), upd.GetPreferences().GetRules()[0].GetPriority())
	assert.Equal(t, int32(2), upd.GetPreferences().GetMaxAttempts())
	assert.NotNil(t, upd.GetPreferences().GetUpdatedAt())

	got, err := s.GetRoutingPreferences(ctx, &merchantv1.GetRoutingPreferencesRequest{MerchantId: m.GetId()})
	require.NoError(t, err)
	assert.Len(t, got.GetPreferences().GetRules(), 2)
	assert.Equal(t, []string{"mock"}, got.GetPreferences().GetFallbackProviders())

	_, err = s.UpdateRoutingPreferences(ctx, &merchantv1.UpdateRoutingPreferencesRequest{Preferences: &merchantv1.RoutingPreferences{
		MerchantId: m.GetId(), Rules: []*merchantv1.RoutingRule{{Priority: 1, Provider: "adyen"}},
	}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.UpdateRoutingPreferences(ctx, &merchantv1.UpdateRoutingPreferencesRequest{Preferences: &merchantv1.RoutingPreferences{
		MerchantId: m.GetId(), Rules: []*merchantv1.RoutingRule{{Priority: 1, Provider: "mock"}, {Priority: 1, Provider: "stripe"}},
	}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.UpdateRoutingPreferences(ctx, &merchantv1.UpdateRoutingPreferencesRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

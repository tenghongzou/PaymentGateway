package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/internal/merchant/app/apptest"
	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

type fixture struct {
	svc   *app.Service
	mem   *apptest.Memory
	clock *apptest.Clock
	seq   int
}

func newFixture(t *testing.T, cfg app.Config) *fixture {
	t.Helper()
	mem := apptest.NewMemory()
	clock := apptest.NewClock(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	mem.Clock = clock
	deps := mem.Deps()
	deps.Clock = clock
	deps.Cipher = domain.PlaintextCipher{}
	cfg.SyncLastUsed = true
	svc, err := app.New(deps, cfg)
	require.NoError(t, err)
	return &fixture{svc: svc, mem: mem, clock: clock}
}

func (f *fixture) merchant(t *testing.T) *domain.Merchant {
	t.Helper()
	f.seq++
	m, err := f.svc.CreateMerchant(context.Background(), app.CreateMerchantInput{
		Name: "Acme", LegalName: "Acme Ltd", Country: "TW", DefaultCurrency: "TWD", ContactEmail: "ops@acme.example",
		ExternalRef: fmt.Sprintf("crm-%s-%d", strings.ToLower(t.Name()), f.seq),
	})
	require.NoError(t, err)
	f.mem.ResetOutbox()
	return m
}

func TestNewRequiresDeps(t *testing.T) {
	_, err := app.New(app.Deps{}, app.Config{})
	require.Error(t, err)
	deps := apptest.NewMemory().Deps()
	_, err = app.New(deps, app.Config{})
	require.Error(t, err, "缺 cipher")
}

func TestCreateMerchant(t *testing.T) {
	f := newFixture(t, app.Config{})
	ctx := context.Background()
	m, err := f.svc.CreateMerchant(ctx, app.CreateMerchantInput{
		Name: "Acme", LegalName: "Acme Ltd", Country: "tw", DefaultCurrency: "twd", ContactEmail: "ops@acme.example", ExternalRef: "crm-1", Metadata: map[string]string{"tier": "gold"},
	})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusActive, m.Status)
	assert.Equal(t, "TW", m.Country)
	assert.Equal(t, f.clock.Now(), m.CreatedAt)

	events := f.mem.Outbox()
	require.Len(t, events, 1)
	assert.Equal(t, app.EventMerchantCreated, events[0].EventType)
	assert.Equal(t, app.AggregateMerchant, events[0].AggregateType)
	assert.Equal(t, m.PublicID(), events[0].AggregateID)
	assert.Equal(t, "application/json", events[0].Headers["content_type"])
	var env app.EventEnvelope
	require.NoError(t, json.Unmarshal(events[0].Payload, &env))
	assert.Equal(t, events[0].ID, env.ID)
	assert.Equal(t, m.PublicID(), env.MerchantID)

	got, err := f.svc.GetMerchant(ctx, m.PublicID())
	require.NoError(t, err)
	assert.Equal(t, "gold", got.Metadata["tier"])
	assert.Equal(t, "crm-1", got.Settings.ExternalRef)

	// external_ref 重複
	_, err = f.svc.CreateMerchant(ctx, app.CreateMerchantInput{Name: "B", LegalName: "B", Country: "TW", DefaultCurrency: "TWD", ContactEmail: "b@b.example", ExternalRef: "crm-1"})
	require.ErrorIs(t, err, domain.ErrAlreadyExists)

	// 驗證錯誤不會寫入
	f.mem.ResetOutbox()
	_, err = f.svc.CreateMerchant(ctx, app.CreateMerchantInput{Name: "", LegalName: "B", Country: "TW", DefaultCurrency: "TWD", ContactEmail: "b@b.example"})
	require.ErrorIs(t, err, domain.ErrParameterMissing)
	assert.Empty(t, f.mem.Outbox())

	_, err = f.svc.GetMerchant(ctx, "mch_01J5X1Y2Z3A4B5C6D7E8F9G0H1")
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.svc.GetMerchant(ctx, "bogus")
	require.ErrorIs(t, err, domain.ErrParameterInvalid)
}

func TestUpdateMerchant(t *testing.T) {
	f := newFixture(t, app.Config{})
	ctx := context.Background()
	m := f.merchant(t)

	_, err := f.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{MerchantID: m.PublicID()})
	require.ErrorIs(t, err, domain.ErrParameterMissing)
	_, err = f.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{MerchantID: m.PublicID(), Fields: []string{"id"}})
	require.ErrorIs(t, err, domain.ErrParameterInvalid)

	f.clock.Advance(time.Minute)
	got, err := f.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{
		MerchantID: m.PublicID(), Fields: []string{app.FieldName, app.FieldStatus, app.FieldMetadata, app.FieldDefaultCurrency},
		Patch: app.MerchantPatch{Name: "Acme 2", Status: domain.StatusSuspended, Metadata: map[string]string{"k": "v"}, DefaultCurrency: "usd"},
	})
	require.NoError(t, err)
	assert.Equal(t, "Acme 2", got.Name)
	assert.Equal(t, domain.StatusSuspended, got.Status)
	assert.Equal(t, "USD", got.DefaultCurrency)
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, f.clock.Now(), got.UpdatedAt)
	assert.Equal(t, []string{app.EventMerchantUpdated}, f.mem.OutboxTypes())

	// 無效轉移：先 close 再 active
	_, err = f.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{MerchantID: m.PublicID(), Fields: []string{app.FieldStatus}, Patch: app.MerchantPatch{Status: domain.StatusClosed}})
	require.NoError(t, err)
	_, err = f.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{MerchantID: m.PublicID(), Fields: []string{app.FieldStatus}, Patch: app.MerchantPatch{Status: domain.StatusActive}})
	require.ErrorIs(t, err, domain.ErrInvalidStateTransition)
	_, err = f.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{MerchantID: m.PublicID(), Fields: []string{app.FieldStatus}, Patch: app.MerchantPatch{Status: domain.StatusPending}})
	require.ErrorIs(t, err, domain.ErrParameterInvalid)

	// repo 錯誤往上傳
	f.mem.FailNext = errors.New("db down")
	_, err = f.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{MerchantID: m.PublicID(), Fields: []string{app.FieldName}, Patch: app.MerchantPatch{Name: "x"}})
	require.ErrorContains(t, err, "db down")
}

func TestListMerchants(t *testing.T) {
	f := newFixture(t, app.Config{})
	ctx := context.Background()
	for i := range 3 {
		_, err := f.svc.CreateMerchant(ctx, app.CreateMerchantInput{Name: "M", LegalName: "L", Country: "TW", DefaultCurrency: "TWD", ContactEmail: "a@b.example"})
		require.NoError(t, err)
		f.clock.Advance(time.Duration(i+1) * time.Second)
	}
	_, err := f.svc.CreateMerchant(ctx, app.CreateMerchantInput{Name: "JP", LegalName: "L", Country: "JP", DefaultCurrency: "JPY", ContactEmail: "a@b.example"})
	require.NoError(t, err)

	all, _, err := f.svc.ListMerchants(ctx, app.ListMerchantsInput{})
	require.NoError(t, err)
	assert.Len(t, all, 4)
	jp, _, err := f.svc.ListMerchants(ctx, app.ListMerchantsInput{Country: "jp"})
	require.NoError(t, err)
	assert.Len(t, jp, 1)
	two, _, err := f.svc.ListMerchants(ctx, app.ListMerchantsInput{Page: app.Page{Size: 2}})
	require.NoError(t, err)
	assert.Len(t, two, 2)
	_, _, err = f.svc.ListMerchants(ctx, app.ListMerchantsInput{Country: "Taiwan"})
	require.Error(t, err)
}

func TestCreateAndVerifyApiKey(t *testing.T) {
	f := newFixture(t, app.Config{MaxAPIKeysPerMode: 2})
	ctx := context.Background()
	m := f.merchant(t)

	out, err := f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeTest, Name: "backend", Scopes: []string{"payments:write", "payments:read"}})
	require.NoError(t, err)
	assert.Regexp(t, `^pk_test_[0-9A-Za-z]{43}$`, out.Plaintext)
	assert.Regexp(t, `^sk_test_[0-9A-Za-z]{43}$`, out.SigningSecret)
	assert.Equal(t, out.Plaintext[:16], out.Key.Prefix)
	assert.NotContains(t, out.Key.KeyHash, out.Plaintext[8:])
	assert.NotEqual(t, out.SigningSecret, out.Key.SigningSecretEnc, "入庫的是加密 / 標記值")
	assert.Equal(t, []string{app.EventAPIKeyCreated}, f.mem.OutboxTypes())
	var env app.EventEnvelope
	require.NoError(t, json.Unmarshal(f.mem.Outbox()[0].Payload, &env))
	raw, _ := json.Marshal(env.Data)
	assert.NotContains(t, string(raw), out.Plaintext[16:], "事件不含明文")
	assert.NotContains(t, string(raw), "sk_test_", "事件不含 secret")

	// 驗證成功
	res, err := f.svc.VerifyApiKey(ctx, out.Plaintext)
	require.NoError(t, err)
	require.True(t, res.Valid)
	assert.Equal(t, out.Key.ID, res.Key.ID)
	assert.Equal(t, m.ID, res.Merchant.ID)
	assert.Equal(t, domain.StatusActive, res.Merchant.Status)
	assert.Equal(t, out.SigningSecret, res.SigningSecret)
	assert.Empty(t, res.PreviousSigningSecret)
	assert.ElementsMatch(t, []string{"payments:write", "payments:read"}, res.Key.Scopes)
	require.NotNil(t, f.mem.Key(out.Key.ID).LastUsedAt, "last_used_at 已更新")
	assert.Equal(t, f.clock.Now(), *f.mem.Key(out.Key.ID).LastUsedAt)

	// 節流：1 分鐘內不再更新
	f.clock.Advance(10 * time.Second)
	_, err = f.svc.VerifyApiKey(ctx, out.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, f.clock.Now().Add(-10*time.Second), *f.mem.Key(out.Key.ID).LastUsedAt)
	f.clock.Advance(time.Minute)
	_, err = f.svc.VerifyApiKey(ctx, out.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, f.clock.Now(), *f.mem.Key(out.Key.ID).LastUsedAt)

	// 錯誤的 key（同 prefix 不同 body）→ not_found
	wrong := out.Plaintext[:len(out.Plaintext)-2] + "zz"
	if wrong == out.Plaintext {
		wrong = out.Plaintext[:len(out.Plaintext)-2] + "yy"
	}
	res, err = f.svc.VerifyApiKey(ctx, wrong)
	require.NoError(t, err)
	assert.False(t, res.Valid)
	assert.Equal(t, app.ReasonNotFound, res.Reason)
	assert.Nil(t, res.Key)

	// 格式錯誤 / 不存在
	for _, bad := range []string{"", "garbage", "pk_test_" + strings.Repeat("A", 43)} {
		res, err = f.svc.VerifyApiKey(ctx, bad)
		require.NoError(t, err)
		assert.False(t, res.Valid, bad)
		assert.Equal(t, app.ReasonNotFound, res.Reason)
	}

	// 上限
	_, err = f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeTest, Name: "second"})
	require.NoError(t, err)
	_, err = f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeTest, Name: "third"})
	require.ErrorIs(t, err, domain.ErrAPIKeyLimit)
	_, err = f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeLive, Name: "live ok"})
	require.NoError(t, err, "不同 mode 各自計算")

	// 輸入驗證
	past := f.clock.Now().Add(-time.Second)
	_, err = f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeLive, Name: "x", ExpiresAt: &past})
	require.ErrorIs(t, err, domain.ErrParameterInvalid)
	_, err = f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: "prod", Name: "x"})
	require.ErrorIs(t, err, domain.ErrParameterInvalid)
	_, err = f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeLive, Name: "x", Scopes: []string{"root"}})
	require.ErrorIs(t, err, domain.ErrParameterInvalid)
	_, err = f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeLive})
	require.ErrorIs(t, err, domain.ErrParameterMissing)
}

func TestVerifyApiKeyRevokedExpiredAndMerchantStatus(t *testing.T) {
	f := newFixture(t, app.Config{})
	ctx := context.Background()
	m := f.merchant(t)

	exp := f.clock.Now().Add(time.Hour)
	expiring, err := f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeLive, Name: "expiring", ExpiresAt: &exp})
	require.NoError(t, err)
	stable, err := f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeLive, Name: "stable"})
	require.NoError(t, err)
	f.mem.ResetOutbox()

	// expired
	f.clock.Advance(2 * time.Hour)
	res, err := f.svc.VerifyApiKey(ctx, expiring.Plaintext)
	require.NoError(t, err)
	assert.False(t, res.Valid)
	assert.Equal(t, app.ReasonExpired, res.Reason)

	// revoke（冪等）
	k, err := f.svc.RevokeApiKey(ctx, m.PublicID(), stable.Key.PublicID(), "leaked")
	require.NoError(t, err)
	require.NotNil(t, k.RevokedAt)
	assert.Equal(t, []string{app.EventAPIKeyRevoked}, f.mem.OutboxTypes())
	var env app.EventEnvelope
	require.NoError(t, json.Unmarshal(f.mem.Outbox()[0].Payload, &env))
	data, _ := json.Marshal(env.Data)
	assert.Contains(t, string(data), `"reason":"leaked"`)
	assert.Contains(t, string(data), `"status":"revoked"`)
	k2, err := f.svc.RevokeApiKey(ctx, m.PublicID(), stable.Key.PublicID(), "again")
	require.NoError(t, err)
	assert.Equal(t, k.RevokedAt, k2.RevokedAt)
	assert.Len(t, f.mem.Outbox(), 1, "重複撤銷不再發事件")

	res, err = f.svc.VerifyApiKey(ctx, stable.Plaintext)
	require.NoError(t, err)
	assert.False(t, res.Valid)
	assert.Equal(t, app.ReasonRevoked, res.Reason)

	// 跨商戶撤銷 → not found
	other := f.merchant(t)
	fresh, err := f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: other.PublicID(), Mode: domain.ModeLive, Name: "o"})
	require.NoError(t, err)
	_, err = f.svc.RevokeApiKey(ctx, m.PublicID(), fresh.Key.PublicID(), "")
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.svc.RevokeApiKey(ctx, m.PublicID(), "nope", "")
	require.ErrorIs(t, err, domain.ErrParameterInvalid)

	// suspended → valid=true 並帶狀態；closed → valid=false merchant_closed
	_, err = f.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{MerchantID: other.PublicID(), Fields: []string{app.FieldStatus}, Patch: app.MerchantPatch{Status: domain.StatusSuspended}})
	require.NoError(t, err)
	res, err = f.svc.VerifyApiKey(ctx, fresh.Plaintext)
	require.NoError(t, err)
	assert.True(t, res.Valid)
	assert.Equal(t, domain.StatusSuspended, res.Merchant.Status)

	_, err = f.svc.UpdateMerchant(ctx, app.UpdateMerchantInput{MerchantID: other.PublicID(), Fields: []string{app.FieldStatus}, Patch: app.MerchantPatch{Status: domain.StatusClosed}})
	require.NoError(t, err)
	res, err = f.svc.VerifyApiKey(ctx, fresh.Plaintext)
	require.NoError(t, err)
	assert.False(t, res.Valid)
	assert.Equal(t, app.ReasonMerchantClosed, res.Reason)

	// closed 商戶不可再建 key
	_, err = f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: other.PublicID(), Mode: domain.ModeLive, Name: "x"})
	require.ErrorIs(t, err, domain.ErrMerchantClosed)
}

func TestVerifyApiKeyPreviousSigningSecret(t *testing.T) {
	f := newFixture(t, app.Config{})
	ctx := context.Background()
	m := f.merchant(t)
	out, err := f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeLive, Name: "k"})
	require.NoError(t, err)

	// 模擬輪替：直接改儲存中的 key（Phase 0 無 rotate rpc）
	k := f.mem.Key(out.Key.ID)
	k.RotateSigningSecret("plain:v1:sk_live_new", f.clock.Now(), 24*time.Hour)
	f.mem.PutKey(k)

	res, err := f.svc.VerifyApiKey(ctx, out.Plaintext)
	require.NoError(t, err)
	require.True(t, res.Valid)
	assert.Equal(t, "sk_live_new", res.SigningSecret)
	assert.Equal(t, out.SigningSecret, res.PreviousSigningSecret)

	f.clock.Advance(25 * time.Hour)
	res, err = f.svc.VerifyApiKey(ctx, out.Plaintext)
	require.NoError(t, err)
	assert.Empty(t, res.PreviousSigningSecret, "視窗過後不再回傳")
}

func TestListApiKeys(t *testing.T) {
	f := newFixture(t, app.Config{})
	ctx := context.Background()
	m := f.merchant(t)
	a, err := f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeLive, Name: "a"})
	require.NoError(t, err)
	_, err = f.svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeTest, Name: "b"})
	require.NoError(t, err)
	_, err = f.svc.RevokeApiKey(ctx, m.PublicID(), a.Key.PublicID(), "")
	require.NoError(t, err)

	keys, _, err := f.svc.ListApiKeys(ctx, app.ListApiKeysInput{MerchantID: m.PublicID()})
	require.NoError(t, err)
	assert.Len(t, keys, 1)
	keys, _, err = f.svc.ListApiKeys(ctx, app.ListApiKeysInput{MerchantID: m.PublicID(), IncludeInactive: true})
	require.NoError(t, err)
	assert.Len(t, keys, 2)
	keys, _, err = f.svc.ListApiKeys(ctx, app.ListApiKeysInput{MerchantID: m.PublicID(), IncludeInactive: true, Mode: domain.ModeLive})
	require.NoError(t, err)
	assert.Len(t, keys, 1)
	_, _, err = f.svc.ListApiKeys(ctx, app.ListApiKeysInput{MerchantID: m.PublicID(), Mode: "x"})
	require.Error(t, err)
}

func TestWebhookEndpointLifecycle(t *testing.T) {
	f := newFixture(t, app.Config{AllowInsecureWebhookURL: true, MaxWebhookEndpoints: 2})
	ctx := context.Background()
	m := f.merchant(t)

	v, err := f.svc.CreateWebhookEndpoint(ctx, app.CreateWebhookEndpointInput{
		MerchantID: m.PublicID(), URL: "http://localhost:3000/hooks", Description: "local", EnabledEvents: []string{"payment.captured"}, Mode: domain.ModeTest, Metadata: map[string]string{"env": "dev"},
	})
	require.NoError(t, err)
	assert.Regexp(t, `^whsec_[0-9A-Za-z]{43}$`, v.Secret)
	assert.Equal(t, domain.EndpointEnabled, v.Endpoint.Status)
	assert.Equal(t, domain.ModeTest, v.Endpoint.Mode)
	assert.Equal(t, []string{app.EventWebhookEndpointCreated}, f.mem.OutboxTypes())
	payload := string(f.mem.Outbox()[0].Payload)
	assert.NotContains(t, payload, "whsec_", "事件不含 secret")
	f.mem.ResetOutbox()

	// 列表（不含 secret / 含 secret）
	list, _, err := f.svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID()})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Empty(t, list[0].Secret)
	list, _, err = f.svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID(), IncludeSecrets: true})
	require.NoError(t, err)
	assert.Equal(t, v.Secret, list[0].Secret)
	assert.Empty(t, list[0].PreviousSecret)

	// 更新 + 輪替
	f.clock.Advance(time.Minute)
	u, err := f.svc.UpdateWebhookEndpoint(ctx, app.UpdateWebhookEndpointInput{
		MerchantID: m.PublicID(), EndpointID: v.Endpoint.PublicID(), Fields: []string{app.FieldURL, app.FieldStatus, app.FieldEnabledEvents},
		Patch: app.WebhookEndpointPatch{URL: "http://localhost:4000/hooks", Status: domain.EndpointDisabled, EnabledEvents: nil}, RotateSecret: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:4000/hooks", u.Endpoint.URL)
	assert.Equal(t, domain.EndpointDisabled, u.Endpoint.Status)
	assert.Equal(t, []string{"*"}, u.Endpoint.EnabledEvents)
	assert.Regexp(t, `^whsec_`, u.Secret)
	assert.NotEqual(t, v.Secret, u.Secret)
	assert.Equal(t, v.Secret, u.PreviousSecret)
	assert.Equal(t, 1, u.Endpoint.Version)
	assert.Equal(t, []string{app.EventWebhookSecretRotated, app.EventWebhookEndpointUpdated}, f.mem.OutboxTypes())
	f.mem.ResetOutbox()

	list, _, err = f.svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID(), IncludeSecrets: true})
	require.NoError(t, err)
	assert.Equal(t, u.Secret, list[0].Secret)
	assert.Equal(t, v.Secret, list[0].PreviousSecret, "輪替視窗內兩把都回傳")

	f.clock.Advance(domain.SecretRotationGrace + time.Second)
	list, _, err = f.svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID(), IncludeSecrets: true})
	require.NoError(t, err)
	assert.Empty(t, list[0].PreviousSecret, "視窗過後不再回傳舊 secret")

	// 只輪替（mask 空）
	u2, err := f.svc.UpdateWebhookEndpoint(ctx, app.UpdateWebhookEndpointInput{MerchantID: m.PublicID(), EndpointID: v.Endpoint.PublicID(), RotateSecret: true})
	require.NoError(t, err)
	assert.NotEmpty(t, u2.Secret)
	assert.Equal(t, []string{app.EventWebhookSecretRotated}, f.mem.OutboxTypes())
	f.mem.ResetOutbox()

	// mask 驗證
	_, err = f.svc.UpdateWebhookEndpoint(ctx, app.UpdateWebhookEndpointInput{MerchantID: m.PublicID(), EndpointID: v.Endpoint.PublicID()})
	require.ErrorIs(t, err, domain.ErrParameterMissing)
	_, err = f.svc.UpdateWebhookEndpoint(ctx, app.UpdateWebhookEndpointInput{MerchantID: m.PublicID(), EndpointID: v.Endpoint.PublicID(), Fields: []string{"mode"}})
	require.ErrorIs(t, err, domain.ErrParameterInvalid)
	_, err = f.svc.UpdateWebhookEndpoint(ctx, app.UpdateWebhookEndpointInput{MerchantID: m.PublicID(), EndpointID: v.Endpoint.PublicID(), Fields: []string{app.FieldURL}, Patch: app.WebhookEndpointPatch{URL: "ftp://x"}})
	require.ErrorIs(t, err, domain.ErrWebhookURLInvalid)
	_, err = f.svc.UpdateWebhookEndpoint(ctx, app.UpdateWebhookEndpointInput{MerchantID: m.PublicID(), EndpointID: v.Endpoint.PublicID(), Fields: []string{app.FieldStatus}, Patch: app.WebhookEndpointPatch{Status: "deleted"}})
	require.ErrorIs(t, err, domain.ErrParameterInvalid)

	// 上限
	_, err = f.svc.CreateWebhookEndpoint(ctx, app.CreateWebhookEndpointInput{MerchantID: m.PublicID(), URL: "https://a.example.com/h", Mode: domain.ModeLive})
	require.NoError(t, err)
	_, err = f.svc.CreateWebhookEndpoint(ctx, app.CreateWebhookEndpointInput{MerchantID: m.PublicID(), URL: "https://b.example.com/h", Mode: domain.ModeLive})
	require.ErrorIs(t, err, domain.ErrWebhookEndpointLimit)
	f.mem.ResetOutbox()

	// 刪除（冪等）→ 之後更新 not found、列表預設不含、include_deleted 含
	require.NoError(t, f.svc.DeleteWebhookEndpoint(ctx, m.PublicID(), v.Endpoint.PublicID()))
	require.NoError(t, f.svc.DeleteWebhookEndpoint(ctx, m.PublicID(), v.Endpoint.PublicID()))
	assert.Equal(t, []string{app.EventWebhookEndpointDeleted}, f.mem.OutboxTypes())
	_, err = f.svc.UpdateWebhookEndpoint(ctx, app.UpdateWebhookEndpointInput{MerchantID: m.PublicID(), EndpointID: v.Endpoint.PublicID(), RotateSecret: true})
	require.ErrorIs(t, err, domain.ErrNotFound)
	list, _, err = f.svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID()})
	require.NoError(t, err)
	assert.Len(t, list, 1)
	list, _, err = f.svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID(), IncludeDeleted: true, IncludeSecrets: true})
	require.NoError(t, err)
	assert.Len(t, list, 2)
	// 刪除後額度釋放
	_, err = f.svc.CreateWebhookEndpoint(ctx, app.CreateWebhookEndpointInput{MerchantID: m.PublicID(), URL: "https://b.example.com/h", Mode: domain.ModeLive})
	require.NoError(t, err)
}

func TestWebhookEndpointProductionURLPolicy(t *testing.T) {
	f := newFixture(t, app.Config{AllowInsecureWebhookURL: false})
	ctx := context.Background()
	m := f.merchant(t)
	_, err := f.svc.CreateWebhookEndpoint(ctx, app.CreateWebhookEndpointInput{MerchantID: m.PublicID(), URL: "http://localhost:3000/hooks", Mode: domain.ModeLive})
	require.ErrorIs(t, err, domain.ErrWebhookURLInvalid)
	_, err = f.svc.CreateWebhookEndpoint(ctx, app.CreateWebhookEndpointInput{MerchantID: m.PublicID(), URL: "https://10.0.0.1/hooks", Mode: domain.ModeLive})
	require.ErrorIs(t, err, domain.ErrWebhookURLInvalid)
	_, err = f.svc.CreateWebhookEndpoint(ctx, app.CreateWebhookEndpointInput{MerchantID: m.PublicID(), URL: "https://hooks.example.com/pg", Mode: domain.ModeLive})
	require.NoError(t, err)
	assert.Empty(t, f.mem.OutboxTypes()[0:0])
}

func TestRoutingPreferences(t *testing.T) {
	f := newFixture(t, app.Config{KnownProviders: []string{"mock", "stripe"}})
	ctx := context.Background()
	m := f.merchant(t)

	def, err := f.svc.GetRoutingPreferences(ctx, m.PublicID())
	require.NoError(t, err)
	assert.Empty(t, def.Rules)
	assert.True(t, def.FailoverEnabled)
	assert.Equal(t, 3, def.MaxAttempts)
	assert.Nil(t, def.UpdatedAt)

	got, err := f.svc.UpdateRoutingPreferences(ctx, app.UpdateRoutingPreferencesInput{
		MerchantID: m.PublicID(),
		Rules: []domain.RoutingRule{
			{Priority: 2, Provider: "stripe", Currencies: []string{"usd"}, Enabled: true},
			{Priority: 1, Provider: "mock", Currencies: []string{"twd"}, Enabled: true},
		},
		FallbackProviders: []string{"mock"}, FailoverEnabled: false, MaxAttempts: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), got.Rules[0].Priority)
	assert.Equal(t, "TWD", got.Rules[0].Currencies[0])
	assert.False(t, got.FailoverEnabled)
	assert.Equal(t, 2, got.MaxAttempts)
	assert.Equal(t, []string{app.EventRoutingPreferencesUpdated}, f.mem.OutboxTypes())

	again, err := f.svc.GetRoutingPreferences(ctx, m.PublicID())
	require.NoError(t, err)
	assert.Len(t, again.Rules, 2)
	assert.False(t, again.FailoverEnabled)
	assert.Equal(t, 2, again.MaxAttempts)
	assert.Equal(t, []string{"mock"}, again.FallbackProviders)
	require.NotNil(t, again.UpdatedAt)

	mm, err := f.svc.GetMerchant(ctx, m.PublicID())
	require.NoError(t, err)
	assert.Equal(t, 2, mm.Settings.MaxAttempts)
	assert.False(t, mm.Settings.EffectiveAllowFailover())

	_, err = f.svc.UpdateRoutingPreferences(ctx, app.UpdateRoutingPreferencesInput{MerchantID: m.PublicID(), Rules: []domain.RoutingRule{{Priority: 1, Provider: "adyen"}}})
	require.ErrorIs(t, err, domain.ErrParameterInvalid)
	_, err = f.svc.UpdateRoutingPreferences(ctx, app.UpdateRoutingPreferencesInput{MerchantID: m.PublicID(), MaxAttempts: 9})
	require.ErrorIs(t, err, domain.ErrParameterInvalid)

	// 整份覆寫：空 rules
	got, err = f.svc.UpdateRoutingPreferences(ctx, app.UpdateRoutingPreferencesInput{MerchantID: m.PublicID(), FailoverEnabled: true})
	require.NoError(t, err)
	assert.Empty(t, got.Rules)
	assert.Equal(t, 1, got.Version)
}

func TestSeedDev(t *testing.T) {
	f := newFixture(t, app.Config{})
	ctx := context.Background()
	r1, err := f.svc.SeedDev(ctx)
	require.NoError(t, err)
	assert.False(t, r1.Reused)
	assert.Regexp(t, `^mch_`, r1.MerchantID)
	assert.Regexp(t, `^pk_test_`, r1.APIKey)
	assert.Regexp(t, `^sk_test_`, r1.SigningSecret)

	res, err := f.svc.VerifyApiKey(ctx, r1.APIKey)
	require.NoError(t, err)
	require.True(t, res.Valid)
	assert.Equal(t, r1.SigningSecret, res.SigningSecret)
	assert.Equal(t, r1.MerchantID, res.Merchant.PublicID())

	r2, err := f.svc.SeedDev(ctx)
	require.NoError(t, err)
	assert.True(t, r2.Reused)
	assert.Equal(t, r1.MerchantID, r2.MerchantID)
	assert.NotEqual(t, r1.APIKey, r2.APIKey)
}

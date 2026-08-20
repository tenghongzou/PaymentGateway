//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/tenghongzou/paymentgateway/internal/merchant/adapter/postgres"
	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
	"github.com/tenghongzou/paymentgateway/migrations"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// startDB 啟動 postgres:16-alpine 並套用 migrations/merchant。
func startDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("pg_merchant"), tcpostgres.WithUsername("merchant_owner"), tcpostgres.WithPassword("merchant_owner"),
		tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	testcontainers.CleanupContainer(t, ctr)
	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	src, err := migrations.Source("merchant")
	require.NoError(t, err)
	require.NoError(t, pgdb.Migrate(ctx, url, "merchant", src))
	v, dirty, err := pgdb.MigrateVersion(ctx, url, "merchant", src)
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, uint(2), v)

	pool, err := pgdb.Connect(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return ctx, pool
}

func newService(t *testing.T, repos *postgres.Repos) *app.Service {
	t.Helper()
	svc, err := app.New(app.Deps{
		Tx: repos.Tx, Merchants: repos.Merchants, APIKeys: repos.APIKeys, Webhooks: repos.Webhooks, Routing: repos.Routing, Outbox: repos.Outbox,
		Clock: app.SystemClock{}, Cipher: domain.PlaintextCipher{},
	}, app.Config{AllowInsecureWebhookURL: true, KnownProviders: []string{"mock", "stripe"}, SyncLastUsed: true, MaxAPIKeysPerMode: 3})
	require.NoError(t, err)
	return svc
}

func TestRepositoriesEndToEnd(t *testing.T) {
	ctx, pool := startDB(t)
	repos := postgres.NewRepos(pool)
	svc := newService(t, repos)

	// ---- merchants ----
	m, err := svc.CreateMerchant(ctx, app.CreateMerchantInput{
		Name: "Acme", LegalName: "Acme Ltd", Country: "tw", DefaultCurrency: "twd", ContactEmail: "ops@acme.example", ExternalRef: "crm-1",
		Metadata: map[string]string{"tier": "gold"},
	})
	require.NoError(t, err)
	got, err := svc.GetMerchant(ctx, m.PublicID())
	require.NoError(t, err)
	assert.Equal(t, "TW", got.Country)
	assert.Equal(t, "TWD", got.DefaultCurrency)
	assert.Equal(t, "Acme Ltd", got.Settings.LegalName)
	assert.Equal(t, "gold", got.Metadata["tier"])
	assert.Equal(t, 0, got.Version)

	_, err = svc.CreateMerchant(ctx, app.CreateMerchantInput{Name: "Dup", LegalName: "D", Country: "TW", DefaultCurrency: "TWD", ContactEmail: "d@d.example", ExternalRef: "crm-1"})
	require.ErrorIs(t, err, domain.ErrAlreadyExists)

	upd, err := svc.UpdateMerchant(ctx, app.UpdateMerchantInput{MerchantID: m.PublicID(), Fields: []string{app.FieldName, app.FieldStatus}, Patch: app.MerchantPatch{Name: "Acme 2", Status: domain.StatusSuspended}})
	require.NoError(t, err)
	assert.Equal(t, 1, upd.Version)
	assert.True(t, upd.UpdatedAt.After(got.UpdatedAt) || upd.UpdatedAt.Equal(got.UpdatedAt))

	// 樂觀鎖：用舊版本更新 → concurrent modification
	stale := *got
	stale.Name = "stale"
	require.ErrorIs(t, repos.Merchants.Update(ctx, &stale), domain.ErrConcurrentModify)
	missing := *got
	missing.ID = uuid.New()
	require.ErrorIs(t, repos.Merchants.Update(ctx, &missing), domain.ErrNotFound)

	// 分頁：再建 3 個，page size 2 → 2 頁 + 1
	for i := range 3 {
		_, err := svc.CreateMerchant(ctx, app.CreateMerchantInput{Name: "M", LegalName: "L", Country: "JP", DefaultCurrency: "JPY", ContactEmail: "a@b.example", Metadata: map[string]string{"i": string(rune('0' + i))}})
		require.NoError(t, err)
	}
	page1, next, err := svc.ListMerchants(ctx, app.ListMerchantsInput{Page: app.Page{Size: 2}})
	require.NoError(t, err)
	assert.Len(t, page1, 2)
	require.NotEmpty(t, next)
	page2, next2, err := svc.ListMerchants(ctx, app.ListMerchantsInput{Page: app.Page{Size: 2, Token: next}})
	require.NoError(t, err)
	assert.Len(t, page2, 2)
	assert.Empty(t, next2)
	assert.NotEqual(t, page1[0].ID, page2[0].ID)
	_, _, err = svc.ListMerchants(ctx, app.ListMerchantsInput{Country: "JP", Page: app.Page{Size: 2, Token: next}})
	require.ErrorIs(t, err, domain.ErrParameterInvalid, "篩選條件改變時舊 cursor 無效")
	jp, _, err := svc.ListMerchants(ctx, app.ListMerchantsInput{Country: "JP"})
	require.NoError(t, err)
	assert.Len(t, jp, 3)
	sus, _, err := svc.ListMerchants(ctx, app.ListMerchantsInput{Status: domain.StatusSuspended})
	require.NoError(t, err)
	assert.Len(t, sus, 1)

	// ---- api keys ----
	k1, err := svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeTest, Name: "k1", Scopes: []string{"payments:read"}})
	require.NoError(t, err)
	cands, err := repos.APIKeys.FindByPrefix(ctx, k1.Key.Prefix)
	require.NoError(t, err)
	require.Len(t, cands, 1)
	assert.Equal(t, k1.Key.ID, cands[0].ID)
	assert.Equal(t, k1.Key.KeyHash, cands[0].KeyHash)
	assert.Equal(t, "plain:v1:"+k1.SigningSecret, cands[0].SigningSecretEnc, "secret 存在 metadata 內部鍵")
	assert.Equal(t, []string{"payments:read"}, cands[0].Scopes)

	// DB CHECK：prefix 與 mode 必須一致
	bad := *k1.Key
	bad.ID = uuid.New()
	bad.Prefix = "pk_live_" + bad.Prefix[8:]
	require.ErrorIs(t, repos.APIKeys.Create(ctx, &bad), domain.ErrParameterInvalid)

	res, err := svc.VerifyApiKey(ctx, k1.Plaintext)
	require.NoError(t, err)
	require.True(t, res.Valid)
	assert.Equal(t, k1.SigningSecret, res.SigningSecret)
	assert.Equal(t, domain.StatusSuspended, res.Merchant.Status)
	touched, err := repos.APIKeys.Get(ctx, m.ID, k1.Key.ID)
	require.NoError(t, err)
	require.NotNil(t, touched.LastUsedAt)

	n, err := repos.APIKeys.CountActive(ctx, m.ID, domain.ModeTest, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	exp := time.Now().Add(time.Hour)
	_, err = svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeTest, Name: "k2", ExpiresAt: &exp})
	require.NoError(t, err)
	_, err = svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeTest, Name: "k3"})
	require.NoError(t, err)
	_, err = svc.CreateApiKey(ctx, app.CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeTest, Name: "k4"})
	require.ErrorIs(t, err, domain.ErrAPIKeyLimit)

	rev, err := svc.RevokeApiKey(ctx, m.PublicID(), k1.Key.PublicID(), "test")
	require.NoError(t, err)
	require.NotNil(t, rev.RevokedAt)
	res, err = svc.VerifyApiKey(ctx, k1.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, app.ReasonRevoked, res.Reason)

	active, _, err := svc.ListApiKeys(ctx, app.ListApiKeysInput{MerchantID: m.PublicID()})
	require.NoError(t, err)
	assert.Len(t, active, 2)
	all, next, err := svc.ListApiKeys(ctx, app.ListApiKeysInput{MerchantID: m.PublicID(), IncludeInactive: true, Page: app.Page{Size: 2}})
	require.NoError(t, err)
	assert.Len(t, all, 2)
	require.NotEmpty(t, next)
	rest, _, err := svc.ListApiKeys(ctx, app.ListApiKeysInput{MerchantID: m.PublicID(), IncludeInactive: true, Page: app.Page{Size: 2, Token: next}})
	require.NoError(t, err)
	assert.Len(t, rest, 1)

	// ---- webhook endpoints ----
	v, err := svc.CreateWebhookEndpoint(ctx, app.CreateWebhookEndpointInput{
		MerchantID: m.PublicID(), URL: "http://localhost:3000/h", Description: "d", EnabledEvents: []string{"payment.captured"}, Mode: domain.ModeTest, Metadata: map[string]string{"env": "dev"},
	})
	require.NoError(t, err)
	stored, err := repos.Webhooks.Get(ctx, m.ID, v.Endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ModeTest, stored.Mode)
	assert.Equal(t, "dev", stored.Metadata["env"])
	assert.NotContains(t, stored.Metadata, "_mode", "內部鍵不外露")
	assert.Equal(t, []string{"payment.captured"}, stored.EnabledEvents)
	assert.Equal(t, "plain:v1:"+v.Secret, stored.SecretCurrentEnc)

	rot, err := svc.UpdateWebhookEndpoint(ctx, app.UpdateWebhookEndpointInput{
		MerchantID: m.PublicID(), EndpointID: v.Endpoint.PublicID(), Fields: []string{app.FieldStatus}, Patch: app.WebhookEndpointPatch{Status: domain.EndpointDisabled}, RotateSecret: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, rot.Endpoint.Version)
	assert.Equal(t, v.Secret, rot.PreviousSecret)
	stored, err = repos.Webhooks.Get(ctx, m.ID, v.Endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.EndpointDisabled, stored.Status)
	require.NotNil(t, stored.SecretRotatedAt)
	assert.NotEmpty(t, stored.SecretPreviousEnc)

	// 樂觀鎖
	staleEp := *v.Endpoint
	require.ErrorIs(t, repos.Webhooks.Update(ctx, &staleEp), domain.ErrConcurrentModify)

	secrets, _, err := svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID(), IncludeSecrets: true})
	require.NoError(t, err)
	require.Len(t, secrets, 1)
	assert.Equal(t, rot.Secret, secrets[0].Secret)
	assert.Equal(t, v.Secret, secrets[0].PreviousSecret)

	live, _, err := svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID(), Mode: domain.ModeLive})
	require.NoError(t, err)
	assert.Empty(t, live)

	require.NoError(t, svc.DeleteWebhookEndpoint(ctx, m.PublicID(), v.Endpoint.PublicID()))
	cnt, err := repos.Webhooks.CountLive(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, cnt)
	visible, _, err := svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID()})
	require.NoError(t, err)
	assert.Empty(t, visible)
	deleted, _, err := svc.ListWebhookEndpoints(ctx, app.ListWebhookEndpointsInput{MerchantID: m.PublicID(), IncludeDeleted: true})
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	assert.True(t, deleted[0].Endpoint.IsDeleted())

	// ---- routing ----
	def, err := svc.GetRoutingPreferences(ctx, m.PublicID())
	require.NoError(t, err)
	assert.Nil(t, def.UpdatedAt)
	assert.Empty(t, def.Rules)
	p1, err := svc.UpdateRoutingPreferences(ctx, app.UpdateRoutingPreferencesInput{
		MerchantID: m.PublicID(), Rules: []domain.RoutingRule{{Priority: 1, Provider: "mock", Currencies: []string{"TWD"}, Enabled: true}}, FailoverEnabled: false, MaxAttempts: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, p1.Version)
	p2, err := svc.UpdateRoutingPreferences(ctx, app.UpdateRoutingPreferencesInput{MerchantID: m.PublicID(), FailoverEnabled: true})
	require.NoError(t, err)
	assert.Equal(t, 1, p2.Version, "upsert 遞增 version")
	got2, err := svc.GetRoutingPreferences(ctx, m.PublicID())
	require.NoError(t, err)
	assert.Empty(t, got2.Rules)
	assert.True(t, got2.FailoverEnabled)
	assert.Equal(t, 3, got2.MaxAttempts)
	require.NotNil(t, got2.UpdatedAt)

	// ---- outbox：事件與業務資料同交易；relay 可送出 ----
	var pending int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&pending))
	assert.Positive(t, pending)
	var types []string
	rows, err := pool.Query(ctx, `SELECT DISTINCT event_type FROM outbox ORDER BY event_type`)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		types = append(types, s)
	}
	rows.Close()
	assert.Subset(t, types, []string{app.EventMerchantCreated, app.EventMerchantUpdated, app.EventAPIKeyCreated, app.EventAPIKeyRevoked,
		app.EventWebhookEndpointCreated, app.EventWebhookSecretRotated, app.EventWebhookEndpointDeleted, app.EventRoutingPreferencesUpdated})

	pub := &capturePublisher{}
	relay := outbox.NewRelay(outbox.RelayConfig{Batcher: outbox.NewPGBatcher(pool), Publisher: pub, Topic: func(outbox.Message) string { return "merchant.events" }, BatchSize: 100})
	total, failed, err := relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, int(pending), total)
	assert.Zero(t, failed)
	assert.Len(t, pub.records, int(pending))
	assert.Equal(t, m.PublicID(), pub.records[0].key, "partition key = merchant public id")
	assert.Equal(t, "application/json", pub.records[0].headers["content_type"])
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&pending))
	assert.Zero(t, pending)

	// outbox 在交易外寫入必須失敗
	_, err = repos.Outbox.Insert(ctx, app.OutboxMessage{AggregateType: "x", AggregateID: "y", EventType: "z", Payload: []byte("{}")})
	require.ErrorIs(t, err, postgres.ErrNoTransaction)

	// ---- rollback：outbox 失敗時業務資料不落地 ----
	before := countRows(t, ctx, pool, "merchants")
	err = repos.Tx.WithinTx(ctx, func(ctx context.Context) error {
		mm, err := domain.NewMerchant(domain.NewMerchantParams{Name: "R", LegalName: "R", Country: "TW", DefaultCurrency: "TWD", ContactEmail: "r@r.example"}, time.Now())
		if err != nil {
			return err
		}
		if err := repos.Merchants.Create(ctx, mm); err != nil {
			return err
		}
		_, err = repos.Outbox.Insert(ctx, app.OutboxMessage{ID: "not-a-uuid", AggregateType: "merchant", AggregateID: mm.PublicID(), EventType: "x", Payload: []byte("{}")})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, before, countRows(t, ctx, pool, "merchants"))

	// ---- seed-dev 走真實 DB ----
	seed, err := svc.SeedDev(ctx)
	require.NoError(t, err)
	res, err = svc.VerifyApiKey(ctx, seed.APIKey)
	require.NoError(t, err)
	assert.True(t, res.Valid)
	assert.Equal(t, seed.SigningSecret, res.SigningSecret)
	seed2, err := svc.SeedDev(ctx)
	require.NoError(t, err)
	assert.True(t, seed2.Reused)
	assert.Equal(t, seed.MerchantID, seed2.MerchantID)
}

func TestAESGCMCipherWithRealDB(t *testing.T) {
	ctx, pool := startDB(t)
	repos := postgres.NewRepos(pool)
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	cipher, err := domain.NewAESGCMCipher(kek)
	require.NoError(t, err)
	svc, err := app.New(app.Deps{
		Tx: repos.Tx, Merchants: repos.Merchants, APIKeys: repos.APIKeys, Webhooks: repos.Webhooks, Routing: repos.Routing, Outbox: repos.Outbox,
		Clock: app.SystemClock{}, Cipher: cipher,
	}, app.Config{SyncLastUsed: true})
	require.NoError(t, err)

	seed, err := svc.SeedDev(ctx)
	require.NoError(t, err)
	var meta string
	require.NoError(t, pool.QueryRow(ctx, `SELECT metadata::text FROM api_keys LIMIT 1`).Scan(&meta))
	assert.Contains(t, meta, "aesgcm:v1:")
	assert.NotContains(t, meta, seed.SigningSecret, "DB 內不得出現明文 secret")
	res, err := svc.VerifyApiKey(ctx, seed.APIKey)
	require.NoError(t, err)
	require.True(t, res.Valid)
	assert.Equal(t, seed.SigningSecret, res.SigningSecret)

	// 沒有 KEK 的服務讀到 aesgcm 密文 → 明確錯誤（fail closed）
	plainSvc, err := app.New(app.Deps{
		Tx: repos.Tx, Merchants: repos.Merchants, APIKeys: repos.APIKeys, Webhooks: repos.Webhooks, Routing: repos.Routing, Outbox: repos.Outbox,
		Clock: app.SystemClock{}, Cipher: domain.PlaintextCipher{},
	}, app.Config{})
	require.NoError(t, err)
	_, err = plainSvc.VerifyApiKey(ctx, seed.APIKey)
	require.ErrorIs(t, err, domain.ErrSecretUnavailable)
	require.ErrorIs(t, err, domain.ErrKEKRequired)
}

type capturedRecord struct {
	topic, key string
	headers    map[string]string
}

type capturePublisher struct{ records []capturedRecord }

func (c *capturePublisher) Publish(_ context.Context, topic, key string, _ []byte, headers map[string]string) error {
	c.records = append(c.records, capturedRecord{topic: topic, key: key, headers: headers})
	return nil
}

func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n))
	return n
}

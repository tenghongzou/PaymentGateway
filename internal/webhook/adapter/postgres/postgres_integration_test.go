//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/migrations"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

func setupStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	pc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("pg_webhook"), tcpostgres.WithUsername("webhook"), tcpostgres.WithPassword("webhook"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	require.NoError(t, err)
	testcontainers.CleanupContainer(t, pc)
	url, err := pc.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	src, err := migrations.Source("webhook")
	require.NoError(t, err)
	require.NoError(t, pgdb.Migrate(ctx, url, "webhook", src))
	pool, err := pgdb.Connect(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return NewStore(pool)
}

type fx struct {
	store *Store
	inbox *Inbox
	ev    *EventRepo
	del   *DeliveryRepo
	merch uuid.UUID
	ep    *domain.Endpoint
}

func newFx(t *testing.T) *fx {
	s := setupStore(t)
	m := uuid.New()
	return &fx{store: s, inbox: NewInbox(), ev: NewEventRepo(s), del: NewDeliveryRepo(s), merch: m,
		ep: &domain.Endpoint{ID: uuid.New(), MerchantID: m, URL: "https://m.example.com/h", Status: domain.EndpointEnabled, Livemode: true}}
}

func (f *fx) newEvent(typ string, at time.Time) *domain.Event {
	id := domain.NewEventID()
	return &domain.Event{ID: id, MerchantID: f.merch, Type: typ, ResourceType: "payment", ResourceID: "pay_1", PaymentID: "pay_1", Livemode: true,
		Payload: []byte(`{"id":"` + domain.EventPublicID(id) + `","type":"` + typ + `","livemode":true}`), OccurredAt: at, CreatedAt: at}
}

// ingest 模擬 app.IngestEvent 的交易：去重 → 事件 → deliveries。
func (f *fx) ingest(t *testing.T, ev *domain.Event, eps ...*domain.Endpoint) (already bool, ds []*domain.Delivery) {
	t.Helper()
	ctx := context.Background()
	err := f.store.InTx(ctx, func(ctx context.Context) error {
		var err error
		already, err = f.inbox.MarkProcessed(ctx, ev.ID, app.ConsumerName)
		if err != nil || already {
			return err
		}
		if err := f.ev.Insert(ctx, ev); err != nil {
			return err
		}
		ds = domain.FanOut(ev, eps, func() time.Time { return ev.CreatedAt })
		return f.del.InsertPending(ctx, ds)
	})
	require.NoError(t, err)
	return already, ds
}

func TestIntegration_IngestDedupAndClaim(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	ev := f.newEvent("payment.captured", now)
	already, ds := f.ingest(t, ev, f.ep)
	assert.False(t, already)
	require.Len(t, ds, 1)
	// 重複 → already，且不再插入。
	already, ds2 := f.ingest(t, ev, f.ep)
	assert.True(t, already)
	assert.Empty(t, ds2)
	// Inbox 在交易外呼叫 → 錯誤。
	_, err := f.inbox.MarkProcessed(ctx, ev.ID, app.ConsumerName)
	require.Error(t, err)
	// 同 (event, endpoint) 再 InsertPending → 靜默略過。
	require.NoError(t, f.del.InsertPending(ctx, []*domain.Delivery{domain.NewDelivery(ev, f.ep, now)}))
	page, err := f.del.List(ctx, app.DeliveryFilter{MerchantID: f.merch})
	require.NoError(t, err)
	require.Len(t, page.Deliveries, 1)

	// 事件可查。
	got, err := f.ev.Get(ctx, f.merch, ev.ID)
	require.NoError(t, err)
	assert.Equal(t, "payment.captured", got.Type)
	assert.True(t, got.Livemode)
	assert.JSONEq(t, string(ev.Payload), string(got.Payload))
	_, err = f.ev.Get(ctx, uuid.New(), ev.ID)
	require.ErrorIs(t, err, domain.ErrEventNotFound)

	// 取件：未到期取不到；到期取到並轉 in_flight、attempt_no=1、帶 payload。
	claimed, err := f.del.ClaimDue(ctx, now.Add(-time.Second), 10)
	require.NoError(t, err)
	assert.Empty(t, claimed)
	claimed, err = f.del.ClaimDue(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	d := claimed[0]
	assert.Equal(t, domain.StatusInFlight, d.Status)
	assert.Equal(t, 1, d.AttemptNo)
	assert.Equal(t, 1, d.Version)
	assert.Equal(t, "payment.captured", d.EventType)
	assert.JSONEq(t, string(ev.Payload), string(d.EventPayload))
	assert.True(t, d.Livemode)
	// 已 in_flight 不會再被取件。
	claimed, err = f.del.ClaimDue(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	assert.Empty(t, claimed)

	// 失敗 → Save（含 attempt）。
	tr, att, err := d.ApplyOutcome(now.Add(time.Second), domain.Outcome{StatusCode: 503, Body: "unavailable", Duration: 250 * time.Millisecond}, 0.5)
	require.NoError(t, err)
	assert.Equal(t, domain.TransitionRetry, tr)
	require.NoError(t, f.del.Save(ctx, d, att))
	// 樂觀鎖：舊版本再寫 → ErrConcurrentModification。
	stale := *d
	stale.Version = d.Version // 未 +1 → WHERE version = $v-1 不成立
	require.ErrorIs(t, f.del.Save(ctx, &stale, nil), pgdb.ErrConcurrentModification)

	got2, err := f.del.Get(ctx, f.merch, d.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, got2.Status)
	assert.Equal(t, 503, *got2.LastResponseStatus)
	assert.Equal(t, "unavailable", *got2.LastResponseBody)
	assert.WithinDuration(t, now.Add(time.Second+time.Minute), got2.NextAttemptAt, time.Millisecond)
	atts, err := f.del.ListAttempts(ctx, d.ID)
	require.NoError(t, err)
	require.Len(t, atts, 1)
	assert.Equal(t, 1, atts[0].AttemptNo)
	assert.Equal(t, 250, atts[0].DurationMS)
	assert.Equal(t, 503, *atts[0].ResponseStatus)
	_, err = f.del.Get(ctx, uuid.New(), d.ID)
	require.ErrorIs(t, err, domain.ErrDeliveryNotFound)

	// 到了 next_attempt_at 再次取件 → attempt_no=2；成功 → succeeded。
	claimed, err = f.del.ClaimDue(ctx, got2.NextAttemptAt, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, 2, claimed[0].AttemptNo)
	_, att, err = claimed[0].ApplyOutcome(got2.NextAttemptAt, domain.Outcome{StatusCode: 200}, 0.5)
	require.NoError(t, err)
	require.NoError(t, f.del.Save(ctx, claimed[0], att))
	got3, err := f.del.Get(ctx, f.merch, d.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSucceeded, got3.Status)
	require.NotNil(t, got3.DeliveredAt)
	atts, err = f.del.ListAttempts(ctx, d.ID)
	require.NoError(t, err)
	assert.Len(t, atts, 2)
}

func TestIntegration_ClaimSkipLockedAndOrdering(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ep2 := &domain.Endpoint{ID: uuid.New(), MerchantID: f.merch, URL: "https://b.example.com/h", Status: domain.EndpointEnabled, Livemode: true}
	for i := 0; i < 3; i++ {
		f.ingest(t, f.newEvent("payment.captured", now.Add(time.Duration(i)*time.Second)), f.ep, ep2)
	}
	// 6 筆 pending；在一個未 commit 的交易內先鎖 2 筆，另一個連線只能拿到其餘 4 筆。
	tx, err := f.store.Pool().Begin(ctx)
	require.NoError(t, err)
	defer func() {
		// 測試中段已明確 Rollback，這裡的收尾 Rollback 會回 ErrTxClosed。
		if rerr := tx.Rollback(ctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			t.Errorf("rollback: %v", rerr)
		}
	}()
	rows, err := tx.Query(ctx, `SELECT id FROM webhook_deliveries WHERE status = 'pending' ORDER BY next_attempt_at LIMIT 2 FOR UPDATE SKIP LOCKED`)
	require.NoError(t, err)
	var locked []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		locked = append(locked, id)
	}
	rows.Close()
	require.Len(t, locked, 2)

	claimed, err := f.del.ClaimDue(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	assert.Len(t, claimed, 4, "被鎖住的 2 筆被 SKIP LOCKED 跳過")
	for _, d := range claimed {
		assert.NotContains(t, locked, d.ID)
	}
	require.NoError(t, tx.Rollback(ctx))
	claimed, err = f.del.ClaimDue(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	assert.Len(t, claimed, 2)
}

func TestIntegration_ReapCancelAndList(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var evs []*domain.Event
	for i := 0; i < 5; i++ {
		typ := "payment.captured"
		if i == 4 {
			typ = "refund.succeeded"
		}
		ev := f.newEvent(typ, now.Add(time.Duration(i)*time.Second))
		f.ingest(t, ev, f.ep)
		evs = append(evs, ev)
	}

	// 分頁：每頁 2 → 3 頁，created_at DESC。
	var seen []uuid.UUID
	token := ""
	for pages := 0; pages < 5; pages++ {
		page, err := f.del.List(ctx, app.DeliveryFilter{MerchantID: f.merch, PageSize: 2, PageToken: token})
		require.NoError(t, err)
		for _, d := range page.Deliveries {
			seen = append(seen, d.ID)
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	assert.Len(t, seen, 5)
	_, err := f.del.List(ctx, app.DeliveryFilter{MerchantID: f.merch, PageToken: "garbage"})
	require.ErrorIs(t, err, app.ErrInvalidPageToken)

	// 篩選：event_type、event_id、endpoint_id、livemode、時間。
	page, err := f.del.List(ctx, app.DeliveryFilter{MerchantID: f.merch, EventType: "refund.succeeded"})
	require.NoError(t, err)
	require.Len(t, page.Deliveries, 1)
	assert.Equal(t, evs[4].ID, page.Deliveries[0].EventID)
	eid := evs[0].ID
	page, err = f.del.List(ctx, app.DeliveryFilter{MerchantID: f.merch, EventID: &eid})
	require.NoError(t, err)
	assert.Len(t, page.Deliveries, 1)
	lm := false
	page, err = f.del.List(ctx, app.DeliveryFilter{MerchantID: f.merch, Livemode: &lm})
	require.NoError(t, err)
	assert.Empty(t, page.Deliveries)
	after := now.Add(3 * time.Second)
	page, err = f.del.List(ctx, app.DeliveryFilter{MerchantID: f.merch, CreatedAfter: &after})
	require.NoError(t, err)
	assert.Len(t, page.Deliveries, 2)

	// 取 2 筆變 in_flight → reaper（updated_at < before）→ failed 且立即可取。
	// 注意：updated_at 由 DB trigger 以 now() 覆寫，因此門檻要以 DB 牆鐘（≈ time.Now）為準。
	claimed, err := f.del.ClaimDue(ctx, now.Add(time.Minute), 2)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	n, err := f.del.ReapStuck(ctx, time.Now().Add(-2*time.Minute), now.Add(3*time.Minute))
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "尚未超時")
	time.Sleep(50 * time.Millisecond)
	n, err = f.del.ReapStuck(ctx, time.Now(), now.Add(3*time.Minute))
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
	page, err = f.del.List(ctx, app.DeliveryFilter{MerchantID: f.merch, Statuses: []domain.DeliveryStatus{domain.StatusFailed}})
	require.NoError(t, err)
	assert.Len(t, page.Deliveries, 2)
	assert.Contains(t, *page.Deliveries[0].LastError, "reaper")

	// 端點停用 → 全部非終態取消。
	n, err = f.del.CancelForEndpoint(ctx, f.ep.ID, now.Add(4*time.Minute), "endpoint returned 410 Gone")
	require.NoError(t, err)
	assert.EqualValues(t, 5, n)
	page, err = f.del.List(ctx, app.DeliveryFilter{MerchantID: f.merch, Statuses: []domain.DeliveryStatus{domain.StatusCanceled}})
	require.NoError(t, err)
	assert.Len(t, page.Deliveries, 5)
	claimed, err = f.del.ClaimDue(ctx, now.Add(time.Hour), 10)
	require.NoError(t, err)
	assert.Empty(t, claimed)
}

func TestIntegration_BodyLengthConstraint(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_, ds := f.ingest(t, f.newEvent("payment.captured", now), f.ep)
	claimed, err := f.del.ClaimDue(ctx, now, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	_ = ds
	big := make([]byte, 10_000)
	for i := range big {
		big[i] = 'x'
	}
	_, att, err := claimed[0].ApplyOutcome(now, domain.Outcome{StatusCode: 500, Body: string(big)}, 0.5)
	require.NoError(t, err)
	// domain 已截斷到 4096，不會撞到 CHECK。
	require.NoError(t, f.del.Save(ctx, claimed[0], att))
	got, err := f.del.Get(ctx, f.merch, claimed[0].ID)
	require.NoError(t, err)
	assert.Len(t, *got.LastResponseBody, 4096)
	assert.NotErrorIs(t, err, pgdb.ErrNotFound)
}

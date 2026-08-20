package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/app/apptest"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

type fixture struct {
	store  *apptest.MemStore
	eps    *apptest.MemEndpoints
	sender *apptest.ScriptedSender
	clock  *apptest.FakeClock
	svc    *app.Service
	merch  uuid.UUID
	ep     *domain.Endpoint
}

func newFixture(t *testing.T, outcomes ...domain.Outcome) *fixture {
	t.Helper()
	f := &fixture{
		store:  apptest.NewMemStore(),
		eps:    apptest.NewMemEndpoints(),
		sender: &apptest.ScriptedSender{Outcomes: outcomes},
		clock:  apptest.NewFakeClock(time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)),
		merch:  uuid.New(),
	}
	f.ep = &domain.Endpoint{
		ID: uuid.New(), MerchantID: f.merch, URL: "https://merchant.example.com/hooks",
		Secrets: []string{"whsec_current", "whsec_previous"}, Status: domain.EndpointEnabled, Livemode: true,
	}
	f.eps.Add(f.ep)
	f.svc = app.New(app.Deps{
		Tx: f.store, Inbox: f.store, Events: &apptest.MemEventRepo{MemStore: f.store}, Deliveries: f.store, Endpoints: f.eps, Disabler: f.eps,
		Sender: f.sender, Clock: f.clock, Policy: domain.StrictPolicy, Rand: func() float64 { return 0.5 },
	})
	return f
}

func (f *fixture) event(typ string, livemode bool) *domain.Event {
	id := domain.NewEventID()
	return &domain.Event{
		ID: id, MerchantID: f.merch, Type: typ, ResourceType: "payment", ResourceID: "pay_1", PaymentID: "pay_1",
		Livemode: livemode, Payload: []byte(`{"id":"` + domain.EventPublicID(id) + `","type":"` + typ + `"}`),
		OccurredAt: f.clock.Now(), CreatedAt: f.clock.Now(),
	}
}

func TestIngestEvent_FanOutAndDedup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// 第二個端點只訂閱 refund.*；第三個是 test mode；第四個停用。
	f.eps.Add(&domain.Endpoint{ID: uuid.New(), MerchantID: f.merch, URL: "https://b.example.com/h", Secrets: []string{"s"}, Status: domain.EndpointEnabled, Livemode: true, EnabledEvents: []string{"refund.*"}})
	f.eps.Add(&domain.Endpoint{ID: uuid.New(), MerchantID: f.merch, URL: "https://c.example.com/h", Secrets: []string{"s"}, Status: domain.EndpointEnabled, Livemode: false})
	f.eps.Add(&domain.Endpoint{ID: uuid.New(), MerchantID: f.merch, URL: "https://d.example.com/h", Secrets: []string{"s"}, Status: domain.EndpointDisabled, Livemode: true})

	ev := f.event("payment.captured", true)
	res, err := f.svc.IngestEvent(ctx, ev)
	require.NoError(t, err)
	assert.False(t, res.Duplicate)
	assert.Equal(t, 1, res.Deliveries)
	assert.Equal(t, 1, f.store.CountByStatus(domain.StatusPending))

	// 重複事件：去重，不再建立 delivery。
	res, err = f.svc.IngestEvent(ctx, ev)
	require.NoError(t, err)
	assert.True(t, res.Duplicate)
	assert.Equal(t, 1, f.store.CountByStatus(domain.StatusPending))

	// refund 事件 → 兩個 live 端點各一筆。
	res, err = f.svc.IngestEvent(ctx, f.event("refund.succeeded", true))
	require.NoError(t, err)
	assert.Equal(t, 2, res.Deliveries)

	// test mode 事件只送 test 端點。
	res, err = f.svc.IngestEvent(ctx, f.event("dispute.opened", false))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Deliveries)

	// 沒有任何端點的商戶：事件仍被記錄（去重），deliveries=0。
	other := f.event("payment.captured", true)
	other.MerchantID = uuid.New()
	res, err = f.svc.IngestEvent(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Deliveries)
	assert.Equal(t, 4, f.store.EventCount())

	// 端點來源失敗 → 回錯誤且不寫入（consumer 會重試）。
	f.eps.Err = errors.New("merchant-service unavailable")
	_, err = f.svc.IngestEvent(ctx, f.event("payment.failed", true))
	require.Error(t, err)
	assert.Equal(t, 4, f.store.EventCount())
}

func TestDispatch_SuccessSignsAndRecordsAttempt(t *testing.T) {
	f := newFixture(t, domain.Outcome{StatusCode: 200, Body: `{"ok":true}`, Duration: 80 * time.Millisecond})
	ctx := context.Background()
	ev := f.event("payment.captured", true)
	_, err := f.svc.IngestEvent(ctx, ev)
	require.NoError(t, err)

	n, err := f.svc.DispatchDue(ctx, 50, 16)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, f.store.CountByStatus(domain.StatusSucceeded))

	require.Len(t, f.sender.Requests, 1)
	req := f.sender.Requests[0]
	assert.Equal(t, f.ep.URL, req.URL)
	assert.Equal(t, ev.Payload, req.Body)
	assert.Equal(t, "application/json", req.Headers["Content-Type"])
	assert.Equal(t, "PaymentGateway-Webhooks/1.0", req.Headers["User-Agent"])
	assert.Equal(t, ev.PublicID(), req.Headers["X-PG-Event-Id"])
	assert.Equal(t, "payment.captured", req.Headers["X-PG-Event-Type"])
	assert.Equal(t, "1", req.Headers["X-PG-Attempt"])
	assert.True(t, strings.HasPrefix(req.Headers["X-PG-Delivery-Id"], "whd_"))
	sigHeader := req.Headers["X-PG-Signature"]
	assert.Equal(t, 2, strings.Count(sigHeader, "v1="), "兩把 secret → 兩個 v1")
	assert.NoError(t, sig.VerifyWebhook([]string{"whsec_current"}, sigHeader, req.Body, f.clock.Now(), 0))
	assert.NoError(t, sig.VerifyWebhook([]string{"whsec_previous"}, sigHeader, req.Body, f.clock.Now(), 0))

	// attempt 紀錄。
	d := f.store.First()
	got, atts, err := f.svc.GetDelivery(ctx, f.merch, d.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSucceeded, got.Status)
	require.Len(t, atts, 1)
	assert.Equal(t, 200, *atts[0].ResponseStatus)
	assert.Equal(t, `{"ok":true}`, *atts[0].ResponseBody)
	assert.Equal(t, 80, atts[0].DurationMS)

	// 再次 dispatch 沒東西。
	n, err = f.svc.DispatchDue(ctx, 50, 16)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestDispatch_TenFailuresToDeadLetter(t *testing.T) {
	f := newFixture(t, domain.Outcome{StatusCode: 503, Body: "nope"})
	ctx := context.Background()
	_, err := f.svc.IngestEvent(ctx, f.event("payment.captured", true))
	require.NoError(t, err)

	for n := 1; n <= domain.MaxAttempts; n++ {
		got, err := f.svc.DispatchDue(ctx, 50, 4)
		require.NoError(t, err)
		require.Equal(t, 1, got, "attempt %d should be dispatched", n)
		d := f.store.First()
		if n < domain.MaxAttempts {
			assert.Equal(t, domain.StatusFailed, d.Status)
			assert.Equal(t, f.clock.Now().Add(domain.Backoff(n+1)), d.NextAttemptAt)
			// 還沒到時間 → 取不到。
			got, err = f.svc.DispatchDue(ctx, 50, 4)
			require.NoError(t, err)
			assert.Equal(t, 0, got)
			f.clock.Set(d.NextAttemptAt)
		} else {
			assert.Equal(t, domain.StatusDeadLetter, d.Status)
		}
	}
	assert.Len(t, f.sender.Requests, domain.MaxAttempts)
	assert.Equal(t, "10", f.sender.Requests[9].Headers["X-PG-Attempt"])
	d := f.store.First()
	atts, _ := f.store.ListAttempts(ctx, d.ID)
	assert.Len(t, atts, 10)

	// 死信不會再被取件。
	f.clock.Advance(48 * time.Hour)
	got, _ := f.svc.DispatchDue(ctx, 50, 4)
	assert.Equal(t, 0, got)

	// 手動重送 → pending、attempt 歸零、下一輪送出（這次成功）。
	f.sender.Outcomes = []domain.Outcome{{StatusCode: 200}}
	rd, err := f.svc.RetryDelivery(ctx, f.merch, d.ID, "idem-1")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPending, rd.Status)
	assert.Equal(t, 0, rd.AttemptNo)
	got, _ = f.svc.DispatchDue(ctx, 50, 4)
	assert.Equal(t, 1, got)
	assert.Equal(t, 1, f.store.CountByStatus(domain.StatusSucceeded))
	assert.Equal(t, "1", f.sender.Requests[10].Headers["X-PG-Attempt"])
}

func TestDispatch_GoneCancelsAndDisablesEndpoint(t *testing.T) {
	f := newFixture(t, domain.Outcome{StatusCode: 410})
	ctx := context.Background()
	_, err := f.svc.IngestEvent(ctx, f.event("payment.captured", true))
	require.NoError(t, err)
	// 同端點另一筆等待中的 delivery 也應被取消。
	f.clock.Advance(time.Second)
	_, err = f.svc.IngestEvent(ctx, f.event("payment.failed", true))
	require.NoError(t, err)

	n, err := f.svc.DispatchDue(ctx, 1, 1) // 只取一筆
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 2, f.store.CountByStatus(domain.StatusCanceled))
	assert.Equal(t, []uuid.UUID{f.ep.ID}, f.eps.Disabled)
	assert.Equal(t, 1, f.eps.Invalidated)
	assert.Len(t, f.sender.Requests, 1)

	// 端點停用後手動重送被拒。
	d := f.store.First()
	_, err = f.svc.RetryDelivery(ctx, f.merch, d.ID, "k")
	assert.ErrorIs(t, err, domain.ErrDeliveryNotRetryable) // canceled 本身不可重送
}

func TestDispatch_RetryAfterAndConnectionError(t *testing.T) {
	f := newFixture(t, domain.Outcome{StatusCode: 429, RetryAfter: 2 * time.Hour}, domain.Outcome{Err: errors.New("dial tcp: connection refused")})
	ctx := context.Background()
	_, err := f.svc.IngestEvent(ctx, f.event("payment.captured", true))
	require.NoError(t, err)

	_, err = f.svc.DispatchDue(ctx, 50, 2)
	require.NoError(t, err)
	d := f.store.First()
	assert.Equal(t, domain.StatusFailed, d.Status)
	assert.Equal(t, f.clock.Now().Add(domain.MaxRetryAfter), d.NextAttemptAt, "Retry-After 上限 1h")

	f.clock.Set(d.NextAttemptAt)
	_, err = f.svc.DispatchDue(ctx, 50, 2)
	require.NoError(t, err)
	d = f.store.Snapshot(d.ID)
	assert.Equal(t, domain.StatusFailed, d.Status)
	assert.Nil(t, d.LastResponseStatus)
	assert.Contains(t, *d.LastError, "connection refused")
	assert.Equal(t, 2, d.AttemptNo)
}

func TestDispatch_EndpointRemovedCancels_SSRFCountsAsAttempt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.svc.IngestEvent(ctx, f.event("payment.captured", true))
	require.NoError(t, err)
	// 端點改成內網 URL（嚴格政策）→ 不送出、記為失敗嘗試。
	f.ep.URL = "http://10.0.0.5/hook"
	_, err = f.svc.DispatchDue(ctx, 50, 2)
	require.NoError(t, err)
	d := f.store.First()
	assert.Equal(t, domain.StatusFailed, d.Status)
	assert.Contains(t, *d.LastError, "not allowed")
	assert.Empty(t, f.sender.Requests)

	// 端點被刪除 → 取消。
	f.eps.ByMerch[f.merch] = nil
	f.clock.Set(d.NextAttemptAt)
	_, err = f.svc.DispatchDue(ctx, 50, 2)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCanceled, f.store.Snapshot(d.ID).Status)
}

func TestDispatch_EndpointLookupErrorReleases(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.svc.IngestEvent(ctx, f.event("payment.captured", true))
	require.NoError(t, err)
	f.eps.Err = errors.New("merchant-service down")
	_, err = f.svc.DispatchDue(ctx, 50, 2)
	require.NoError(t, err)
	d := f.store.First()
	assert.Equal(t, domain.StatusFailed, d.Status)
	assert.Equal(t, 0, d.AttemptNo, "未真正嘗試，不計次")
	assert.Equal(t, f.clock.Now().Add(app.EndpointLookupRetryIn), d.NextAttemptAt)
	atts, _ := f.store.ListAttempts(ctx, d.ID)
	assert.Empty(t, atts)
}

func TestReapStuckInFlight(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.svc.IngestEvent(ctx, f.event("payment.captured", true))
	require.NoError(t, err)
	claimed, err := f.store.ClaimDue(ctx, f.clock.Now(), 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, 1, f.store.CountByStatus(domain.StatusInFlight))

	n, err := f.svc.ReapStuckInFlight(ctx, 2*time.Minute)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "未超時不回收")

	f.clock.Advance(3 * time.Minute)
	n, err = f.svc.ReapStuckInFlight(ctx, 2*time.Minute)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	assert.Equal(t, 1, f.store.CountByStatus(domain.StatusFailed))
	// 回收後可再次取件。
	got, _ := f.svc.DispatchDue(ctx, 50, 2)
	assert.Equal(t, 1, got)
}

func TestRetryDelivery_Preconditions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.svc.IngestEvent(ctx, f.event("payment.captured", true))
	require.NoError(t, err)
	d := f.store.First()
	_, err = f.svc.RetryDelivery(ctx, f.merch, d.ID, "")
	assert.ErrorIs(t, err, domain.ErrIdempotencyKeyMissing)
	_, err = f.svc.RetryDelivery(ctx, f.merch, d.ID, "k1")
	assert.ErrorIs(t, err, domain.ErrDeliveryNotRetryable, "pending 不可重送")
	_, err = f.svc.RetryDelivery(ctx, uuid.New(), d.ID, "k1")
	assert.ErrorIs(t, err, domain.ErrDeliveryNotFound, "跨商戶 NOT_FOUND")

	// 送成功後允許重放。
	_, err = f.svc.DispatchDue(ctx, 50, 2)
	require.NoError(t, err)
	rd, err := f.svc.RetryDelivery(ctx, f.merch, d.ID, "k2")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPending, rd.Status)
	// 同 key 重複 → 回同一筆現況，不再重置。
	again, err := f.svc.RetryDelivery(ctx, f.merch, d.ID, "k2")
	require.NoError(t, err)
	assert.Equal(t, rd.ID, again.ID)
	// 同 key 不同 delivery → 拒絕。
	_, err = f.svc.RetryDelivery(ctx, f.merch, uuid.New(), "k2")
	assert.Error(t, err)

	// 端點停用 → 拒絕。
	_, err = f.svc.DispatchDue(ctx, 50, 2)
	require.NoError(t, err)
	f.ep.Status = domain.EndpointDisabled
	_, err = f.svc.RetryDelivery(ctx, f.merch, d.ID, "k3")
	assert.ErrorIs(t, err, domain.ErrEndpointUnavailable)
}

func TestListDeliveriesAndEventTypes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := f.svc.IngestEvent(ctx, f.event("payment.captured", true))
		require.NoError(t, err)
	}
	page, err := f.svc.ListDeliveries(ctx, app.DeliveryFilter{MerchantID: f.merch, Statuses: []domain.DeliveryStatus{domain.StatusPending}})
	require.NoError(t, err)
	assert.Len(t, page.Deliveries, 3)
	page, err = f.svc.ListDeliveries(ctx, app.DeliveryFilter{MerchantID: uuid.New()})
	require.NoError(t, err)
	assert.Empty(t, page.Deliveries)
	assert.Len(t, f.svc.ListEventTypes(), 14)
}

func TestEndpointCache(t *testing.T) {
	src := apptest.NewMemEndpoints()
	m := uuid.New()
	src.Add(&domain.Endpoint{ID: uuid.New(), MerchantID: m, Status: domain.EndpointEnabled})
	clock := apptest.NewFakeClock(time.Now())
	c := app.NewEndpointCache(src, time.Minute, clock)
	ctx := context.Background()

	eps, err := c.ListEndpoints(ctx, m)
	require.NoError(t, err)
	assert.Len(t, eps, 1)
	_, _ = c.ListEndpoints(ctx, m)
	_, _ = c.GetEndpoint(ctx, m, eps[0].ID)
	assert.Equal(t, 1, src.Calls, "TTL 內只打一次上游")

	clock.Advance(61 * time.Second)
	_, _ = c.ListEndpoints(ctx, m)
	assert.Equal(t, 2, src.Calls, "TTL 過期重新取得")

	// 上游失敗 → 回過期資料。
	clock.Advance(61 * time.Second)
	src.Err = errors.New("down")
	eps, err = c.ListEndpoints(ctx, m)
	require.NoError(t, err)
	assert.Len(t, eps, 1)

	// Invalidate 後下次必打上游；上游仍失敗且無快取 → 錯誤。
	c.Invalidate(m)
	_, err = c.ListEndpoints(ctx, m)
	assert.Error(t, err)
	// 不存在的端點回 (nil, nil)。
	src.Err = nil
	ep, err := c.GetEndpoint(ctx, m, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, ep)
}

func TestDispatcherWorkerStopsOnCancel(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&app.Dispatcher{Svc: f.svc, Interval: 5 * time.Millisecond, Batch: 10, Concurrency: 2}).Run(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not stop")
	}
	rctx, rcancel := context.WithCancel(context.Background())
	rdone := make(chan error, 1)
	go func() { rdone <- (&app.Reaper{Svc: f.svc, Interval: 5 * time.Millisecond}).Run(rctx) }()
	time.Sleep(15 * time.Millisecond)
	rcancel()
	select {
	case err := <-rdone:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reaper did not stop")
	}
}

//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tenghongzou/paymentgateway/internal/payment/app"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/migrations"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

func TestRepoIntegration(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("pg_payment"), tcpostgres.WithUsername("payment_owner"), tcpostgres.WithPassword("payment_owner"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	require.NoError(t, err)
	testcontainers.CleanupContainer(t, pgc)
	url, err := pgc.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	src, err := migrations.Source("payment")
	require.NoError(t, err)
	require.NoError(t, pgdb.Migrate(ctx, url, "payment", src))
	v, dirty, err := pgdb.MigrateVersion(ctx, url, "payment", src)
	require.NoError(t, err)
	assert.False(t, dirty)
	assert.GreaterOrEqual(t, v, uint(3))

	pool, err := pgdb.Connect(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	repo := NewRepo(pool)
	txm := NewTxManager(pool)
	ob := NewOutboxStore()
	merchant := ids.NewUUID().String()
	now := time.Now().UTC().Truncate(time.Microsecond)

	p, created, err := domain.NewPayment(domain.NewPaymentParams{MerchantID: merchant, IdempotencyKey: "k1", RequestHash: "h1", Amount: money.MustNew(1000, "TWD"), PaymentMethodType: "card", PaymentMethod: domain.PaymentMethodDetails{TokenProvider: "mock"}, Customer: domain.Customer{ID: "cus_1"}, Metadata: map[string]string{"a": "b"}}, now)
	require.NoError(t, err)
	att, err := p.StartAttempt("mock", "default", now)
	require.NoError(t, err)

	// tx1
	require.NoError(t, txm.WithinTx(ctx, func(ctx context.Context) error {
		if e := repo.CreatePayment(ctx, p); e != nil {
			return e
		}
		if e := repo.InsertAttempt(ctx, att); e != nil {
			return e
		}
		if e := repo.AppendEvents(ctx, p, []domain.Event{created}, "trace"); e != nil {
			return e
		}
		return ob.Insert(ctx, outbox.Message{AggregateType: "payment", AggregateID: p.PublicID, EventType: created.Type, Payload: []byte("x")})
	}))
	// 唯一索引。
	dup := *p
	dup.ID = ids.NewUUID().String()
	dup.PublicID = ids.New("pay")
	require.ErrorIs(t, txm.WithinTx(ctx, func(ctx context.Context) error { return repo.CreatePayment(ctx, &dup) }), app.ErrDuplicateIdempotencyKey)

	got, err := repo.GetPayment(ctx, merchant, p.PublicID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCreated, got.Status)
	assert.Equal(t, "h1", got.IdempotencyRequestHash)
	assert.Equal(t, "cus_1", got.Customer.ID)
	assert.Equal(t, "mock", got.PaymentMethodDetails.TokenProvider)
	require.Len(t, got.Attempts, 1)
	assert.Equal(t, "default", got.Attempts[0].RouteReason)
	_, err = repo.GetPayment(ctx, ids.NewUUID().String(), p.PublicID)
	require.ErrorIs(t, err, domain.ErrPaymentNotFound)
	byKey, err := repo.GetPaymentByIdempotencyKey(ctx, merchant, "k1")
	require.NoError(t, err)
	assert.Equal(t, p.PublicID, byKey.PublicID)

	// requires_action 的 next_action 落在 response_snapshot。
	att.MarkRequiresAction("ref", &domain.NextAction{Type: "redirect", URL: "http://x", ExpiresAt: now.Add(30 * time.Minute)}, now)
	evRA, err := p.RequireAction("ref", now.Add(30*time.Minute), now)
	require.NoError(t, err)
	require.NoError(t, txm.WithinTx(ctx, func(ctx context.Context) error {
		if e := repo.UpdatePayment(ctx, p, p.Version-1); e != nil {
			return e
		}
		if e := repo.UpdateAttempt(ctx, att); e != nil {
			return e
		}
		return repo.AppendEvents(ctx, p, []domain.Event{evRA}, "")
	}))
	got = must(repo.GetPayment(ctx, merchant, p.PublicID))
	assert.Equal(t, "http://x", got.Attempts[0].NextAction.URL)

	// 樂觀鎖：錯誤版本 → ErrConcurrentModification。
	require.ErrorIs(t, txm.WithinTx(ctx, func(ctx context.Context) error { return repo.UpdatePayment(ctx, p, 99) }), pgdb.ErrConcurrentModification)

	// authorize + capture。
	att.MarkApproved("ref", now)
	evA, err := p.Authorize(domain.AuthorizeParams{Provider: "mock", ProviderReference: "ref", Details: &domain.PaymentMethodDetails{Brand: "visa", Last4: "4242"}}, now)
	require.NoError(t, err)
	evC, err := p.Capture(p.Amount, 59, now)
	require.NoError(t, err)
	require.NoError(t, txm.WithinTx(ctx, func(ctx context.Context) error {
		cur, gerr := repo.GetPaymentForUpdate(ctx, merchant, p.PublicID)
		if gerr != nil {
			return gerr
		}
		if e := repo.UpdatePayment(ctx, p, cur.Version); e != nil {
			return e
		}
		if e := repo.UpdateAttempt(ctx, att); e != nil {
			return e
		}
		return repo.AppendEvents(ctx, p, []domain.Event{evA, evC}, "")
	}))
	got = must(repo.GetPayment(ctx, merchant, p.PublicID))
	assert.Equal(t, domain.StatusCaptured, got.Status)
	assert.Equal(t, int64(1000), got.AmountCaptured.AmountMinor)
	assert.Equal(t, "visa", got.PaymentMethodDetails.Brand)
	assert.Equal(t, domain.AttemptApproved, got.Attempts[0].Status)

	// 事件 seq = version。
	var seqs []int
	rows, err := pool.Query(ctx, `SELECT seq FROM payment_events WHERE payment_id = $1::uuid ORDER BY seq`, p.ID)
	require.NoError(t, err)
	for rows.Next() {
		var s int
		require.NoError(t, rows.Scan(&s))
		seqs = append(seqs, s)
	}
	rows.Close()
	assert.Equal(t, []int{1, 2, 3, 4}, seqs)

	// refunds。
	r1, err := domain.NewRefund(p, "rk1", money.MustNew(400, "TWD"), "requested_by_customer", nil, now)
	require.NoError(t, err)
	require.NoError(t, txm.WithinTx(ctx, func(ctx context.Context) error {
		cur, gerr := repo.GetPaymentForUpdate(ctx, merchant, p.PublicID)
		if gerr != nil {
			return gerr
		}
		base := cur.Version
		ev, terr := cur.ReserveRefund(r1, now)
		if terr != nil {
			return terr
		}
		if e := repo.CreateRefund(ctx, r1); e != nil {
			return e
		}
		if e := repo.UpdatePayment(ctx, cur, base); e != nil {
			return e
		}
		return repo.AppendEvents(ctx, cur, []domain.Event{ev}, "")
	}))
	dupR := *r1
	dupR.ID = ids.NewUUID().String()
	dupR.PublicID = ids.New("re")
	require.ErrorIs(t, txm.WithinTx(ctx, func(ctx context.Context) error { return repo.CreateRefund(ctx, &dupR) }), app.ErrDuplicateIdempotencyKey)
	gr, err := repo.GetRefund(ctx, merchant, r1.PublicID)
	require.NoError(t, err)
	assert.Equal(t, p.PublicID, gr.PaymentPublicID)
	assert.Equal(t, domain.RefundPending, gr.Status)
	require.NoError(t, r1.Succeed("mock_re", now))
	require.NoError(t, txm.WithinTx(ctx, func(ctx context.Context) error {
		cur, gerr := repo.GetPaymentForUpdate(ctx, merchant, p.PublicID)
		if gerr != nil {
			return gerr
		}
		base := cur.Version
		ev, terr := cur.MarkRefunded(r1, now)
		if terr != nil {
			return terr
		}
		if e := repo.UpdateRefund(ctx, r1, 0); e != nil {
			return e
		}
		if e := repo.UpdatePayment(ctx, cur, base); e != nil {
			return e
		}
		return repo.AppendEvents(ctx, cur, []domain.Event{ev}, "")
	}))
	got = must(repo.GetPayment(ctx, merchant, p.PublicID))
	assert.Equal(t, domain.StatusPartiallyRefunded, got.Status)
	byKey2, err := repo.GetRefundByIdempotencyKey(ctx, merchant, "rk1")
	require.NoError(t, err)
	assert.Equal(t, domain.RefundSucceeded, byKey2.Status)
	require.ErrorIs(t, txm.WithinTx(ctx, func(ctx context.Context) error { return repo.UpdateRefund(ctx, r1, 42) }), pgdb.ErrConcurrentModification)

	// list + cursor。
	for i := range 3 {
		q, ev, nerr := domain.NewPayment(domain.NewPaymentParams{MerchantID: merchant, IdempotencyKey: "list-" + string(rune('a'+i)), Amount: money.MustNew(100, "TWD"), PaymentMethodType: "card"}, now.Add(time.Duration(i+1)*time.Second))
		require.NoError(t, nerr)
		require.NoError(t, txm.WithinTx(ctx, func(ctx context.Context) error {
			if e := repo.CreatePayment(ctx, q); e != nil {
				return e
			}
			return repo.AppendEvents(ctx, q, []domain.Event{ev}, "")
		}))
	}
	page1, next, err := repo.ListPayments(ctx, merchant, app.ListFilter{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, page1, 2)
	assert.NotEmpty(t, next)
	page2, next2, err := repo.ListPayments(ctx, merchant, app.ListFilter{Limit: 2, Cursor: next})
	require.NoError(t, err)
	assert.Len(t, page2, 2)
	assert.Empty(t, next2)
	assert.NotEqual(t, page1[0].PublicID, page2[0].PublicID)
	created3, _, err := repo.ListPayments(ctx, merchant, app.ListFilter{Statuses: []domain.Status{domain.StatusCreated}})
	require.NoError(t, err)
	assert.Len(t, created3, 3)

	// outbox relay 一輪（fake publisher）。
	relay := outbox.NewRelay(outbox.RelayConfig{Batcher: outbox.NewPGBatcher(pool), Publisher: publisherFunc(func(context.Context, string, string, []byte, map[string]string) error { return nil }), Topic: func(outbox.Message) string { return "payment.events" }})
	total, failed, err := relay.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Equal(t, 0, failed)
	lag, pending, err := outbox.PendingLag(ctx, pool)
	require.NoError(t, err)
	assert.Zero(t, pending)
	assert.Zero(t, lag)

	// inbox 去重。
	require.NoError(t, pgdb.WithTx(ctx, pool, func(tx pgxTx) error {
		already, merr := outbox.NewInbox().MarkProcessed(ctx, tx, ids.NewUUID().String(), "test")
		assert.False(t, already)
		return merr
	}))
}

type publisherFunc func(ctx context.Context, topic, key string, value []byte, headers map[string]string) error

func (f publisherFunc) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	return f(ctx, topic, key, value, headers)
}

type pgxTx = pgx.Tx

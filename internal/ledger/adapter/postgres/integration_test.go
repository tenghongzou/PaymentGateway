//go:build integration

package postgres

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	"github.com/tenghongzou/paymentgateway/internal/ledger/app"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/migrations"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// startPostgres 啟動 postgres:16 並套用 migrations/ledger，回傳 pool（整個 package 共用一個容器）。
func startPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("pg_ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	testcontainers.CleanupContainer(t, pg)
	url, err := pg.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	src, err := migrations.Source("ledger")
	require.NoError(t, err)
	require.NoError(t, pgdb.Migrate(ctx, url, "ledger", src))

	pool, err := pgdb.Connect(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func twd(n int64) money.Money { return money.Money{AmountMinor: n, Currency: "TWD"} }

func newService(pool *pgxpool.Pool) *app.Service {
	return app.NewService(app.Deps{
		Tx: NewTxRunner(pool), Accounts: NewAccountRepo(pool), Journals: NewJournalRepo(pool), Balances: NewBalanceRepo(pool),
		Inbox: NewInbox(), Outbox: NewOutboxStore(), Logger: slog.New(slog.DiscardHandler), Policy: domain.Policy{},
	})
}

func captureEvent(m uuid.UUID, amount, fee int64) domain.PaymentEvent {
	return domain.PaymentEvent{
		EventID: uuid.New(), EventPublicID: ids.New(ids.PrefixEvent), Type: domain.EventPaymentCaptured, OccurredAt: time.Now().UTC(),
		MerchantID: m, PaymentID: "pay_" + uuid.NewString()[:8], Livemode: true, Provider: "stripe", Amount: twd(amount), Fee: twd(fee),
	}
}

func count(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, pool.QueryRow(ctx, sql, args...).Scan(&n))
	return n
}

func TestIntegration(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	t.Run("PostJournal creates accounts, entries, balances and outbox", func(t *testing.T) {
		svc := newService(pool)
		m := uuid.New()
		j, err := domain.TemplateFor(captureEvent(m, 1000, 33), domain.Policy{})
		require.NoError(t, err)
		posted, replayed, err := svc.PostJournal(ctx, j)
		require.NoError(t, err)
		assert.False(t, replayed)

		got, err := svc.GetJournal(ctx, posted.ID, m)
		require.NoError(t, err)
		assert.Equal(t, posted.PublicID, got.PublicID)
		assert.Equal(t, domain.TemplateJCAP, got.Template)
		assert.Equal(t, domain.SourcePaymentEvent, got.SourceType)
		assert.True(t, got.Livemode)
		assert.WithinDuration(t, j.EffectiveAt, got.EffectiveAt, time.Microsecond)
		require.Len(t, got.Entries, 3)
		assert.Equal(t, domain.KindPSPReceivable, got.Entries[0].Account.Kind())
		assert.Equal(t, "stripe", got.Entries[0].Account.Qualifier())
		assert.Equal(t, m, got.Entries[1].Account.MerchantID)

		// balances 由 trigger 維護
		mp, err := NewAccountRepo(pool).GetByKey(ctx, domain.MerchantPayable(m, "TWD", true))
		require.NoError(t, err)
		b, err := svc.GetBalance(ctx, mp.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(967), b.Balance)
		assert.Equal(t, int64(967), b.TotalCredit)
		assert.Equal(t, int64(0), b.TotalDebit)
		assert.Equal(t, int64(1), b.EntryCount)
		require.NotNil(t, b.AsOfJournalID)
		assert.Equal(t, posted.ID, *b.AsOfJournalID)

		mbs, err := svc.GetMerchantBalances(ctx, m, "TWD", true)
		require.NoError(t, err)
		require.Len(t, mbs, 1)
		assert.Equal(t, int64(967), mbs[0].Available)

		// outbox
		assert.Equal(t, int64(1), count(ctx, t, pool, `SELECT count(*) FROM outbox WHERE event_type = $1 AND aggregate_id = $2`, app.EventJournalPosted, app.MerchantPublicID(m)))

		// P6：同一 event_id 重放
		again, replayed, err := svc.PostJournal(ctx, captureJournalWithEvent(t, m, j.EventID))
		require.NoError(t, err)
		assert.True(t, replayed)
		assert.Equal(t, posted.ID, again.ID)
		assert.Equal(t, int64(1), count(ctx, t, pool, `SELECT count(*) FROM journals WHERE event_id = $1`, j.EventID))
		b, err = svc.GetBalance(ctx, mp.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(967), b.Balance)
	})

	t.Run("deferred trigger rejects unbalanced journal at commit", func(t *testing.T) {
		m := uuid.New()
		accounts := NewAccountRepo(pool)
		journals := NewJournalRepo(pool)
		tx := NewTxRunner(pool)
		err := tx.RunInTx(ctx, func(ctx context.Context) error {
			psp, _, err := accounts.EnsureAccount(ctx, domain.PSPReceivable("stripe", "TWD", true))
			require.NoError(t, err)
			mp, _, err := accounts.EnsureAccount(ctx, domain.MerchantPayable(m, "TWD", true))
			require.NoError(t, err)
			id := ids.NewUUID()
			j := &domain.Journal{
				ID: id, PublicID: ids.Format(ids.PrefixJournal, id), EventID: uuid.New(), MerchantID: m, Livemode: true,
				ReferenceType: domain.RefAdjustment, ReferenceID: "x", PostedAt: time.Now().UTC(),
				Entries: []domain.Entry{
					{ID: ids.NewUUID(), AccountID: psp.ID, Account: psp.Key, Direction: domain.Debit, Amount: twd(100)},
					{ID: ids.NewUUID(), AccountID: mp.ID, Account: mp.Key, Direction: domain.Credit, Amount: twd(90)},
				},
			}
			// 繞過 domain.Validate 直接插入：DB 必須在 commit 擋下
			return journals.Insert(ctx, j)
		})
		require.Error(t, err)
		require.ErrorIs(t, err, domain.ErrJournalUnbalanced)
		assert.Equal(t, int64(0), count(ctx, t, pool, `SELECT count(*) FROM journals WHERE merchant_id = $1`, m))
		// 餘額未被污染（交易整體 rollback）
		assert.Equal(t, int64(0), count(ctx, t, pool, `SELECT count(*) FROM accounts WHERE merchant_id = $1`, m))
	})

	t.Run("journal without entries is rejected", func(t *testing.T) {
		m := uuid.New()
		journals := NewJournalRepo(pool)
		err := NewTxRunner(pool).RunInTx(ctx, func(ctx context.Context) error {
			id := ids.NewUUID()
			return journals.Insert(ctx, &domain.Journal{
				ID: id, PublicID: ids.Format(ids.PrefixJournal, id), EventID: uuid.New(), MerchantID: m, Livemode: true,
				ReferenceType: domain.RefAdjustment, ReferenceID: "x", PostedAt: time.Now().UTC(),
			})
		})
		assert.ErrorIs(t, err, domain.ErrJournalTooFewEntries)
	})

	t.Run("append-only: UPDATE / DELETE on journals and entries fail", func(t *testing.T) {
		svc := newService(pool)
		m := uuid.New()
		j, err := domain.TemplateFor(captureEvent(m, 500, 0), domain.Policy{})
		require.NoError(t, err)
		posted, _, err := svc.PostJournal(ctx, j)
		require.NoError(t, err)

		_, err = pool.Exec(ctx, `UPDATE journals SET description = 'x' WHERE id = $1`, posted.ID)
		require.ErrorIs(t, translateError(err), ErrAppendOnly)
		_, err = pool.Exec(ctx, `DELETE FROM journals WHERE id = $1`, posted.ID)
		require.ErrorIs(t, translateError(err), ErrAppendOnly)
		_, err = pool.Exec(ctx, `UPDATE entries SET amount = 1 WHERE journal_id = $1`, posted.ID)
		require.ErrorIs(t, translateError(err), ErrAppendOnly)
		_, err = pool.Exec(ctx, `DELETE FROM entries WHERE journal_id = $1`, posted.ID)
		assert.ErrorIs(t, translateError(err), ErrAppendOnly)
	})

	t.Run("frozen account rejected by trigger", func(t *testing.T) {
		svc := newService(pool)
		m := uuid.New()
		_, _, err := svc.PostJournal(ctx, mustTemplate(t, captureEvent(m, 100, 0)))
		require.NoError(t, err)
		mp, err := NewAccountRepo(pool).GetByKey(ctx, domain.MerchantPayable(m, "TWD", true))
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE accounts SET status = 'frozen' WHERE id = $1`, mp.ID)
		require.NoError(t, err)
		_, _, err = svc.PostJournal(ctx, mustTemplate(t, captureEvent(m, 100, 0)))
		assert.ErrorIs(t, err, domain.ErrAccountInactive)
	})

	t.Run("reversal links and is single-use", func(t *testing.T) {
		svc := newService(pool)
		m := uuid.New()
		orig, _, err := svc.PostJournal(ctx, mustTemplate(t, captureEvent(m, 1000, 30)))
		require.NoError(t, err)
		rev, _, err := svc.ReverseJournal(ctx, orig.ID, "ops-"+uuid.NewString(), "fix")
		require.NoError(t, err)
		got, err := svc.GetJournal(ctx, orig.ID, m)
		require.NoError(t, err)
		require.NotNil(t, got.ReversedBy)
		assert.Equal(t, rev.ID, *got.ReversedBy)
		_, _, err = svc.ReverseJournal(ctx, orig.ID, "ops-"+uuid.NewString(), "again")
		require.ErrorIs(t, err, domain.ErrJournalAlreadyReversed)

		mp, err := NewAccountRepo(pool).GetByKey(ctx, domain.MerchantPayable(m, "TWD", true))
		require.NoError(t, err)
		b, err := svc.GetBalance(ctx, mp.ID)
		require.NoError(t, err)
		assert.Zero(t, b.Balance)
		assert.Equal(t, int64(970), b.TotalDebit)
		assert.Equal(t, int64(970), b.TotalCredit)

		// 試算平衡（v_balance_drift 應為 0 列）
		assert.Equal(t, int64(0), count(ctx, t, pool, `SELECT count(*) FROM v_balance_drift`))
	})

	t.Run("HandlePaymentEvent end-to-end with protobuf and dedupe", func(t *testing.T) {
		svc := newService(pool)
		m := uuid.New()
		eventID := ids.New(ids.PrefixEvent)
		ev := &paymentv1.PaymentEvent{
			EventId: eventID, EventType: paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED,
			OccurredAt: timestamppb.Now(), MerchantId: ids.Format(ids.PrefixMerchant, m), PaymentId: "pay_e2e", Livemode: false,
			Payload: &paymentv1.PaymentEvent_PaymentCaptured{PaymentCaptured: &paymentv1.PaymentCaptured{
				Amount: &commonv1.Money{AmountMinor: 2000, Currency: "TWD"}, Fee: &commonv1.Money{AmountMinor: 50, Currency: "TWD"}, Provider: "mock",
			}},
		}
		raw, err := proto.Marshal(ev)
		require.NoError(t, err)
		rec := eventbus.Record{Topic: eventbus.TopicPaymentEvents, Key: "pay_e2e", Value: raw, Headers: map[string]string{eventbus.HeaderEventID: eventID}}

		require.NoError(t, svc.HandlePaymentEvent(ctx, rec))
		require.NoError(t, svc.HandlePaymentEvent(ctx, rec)) // 重送

		evUUID, err := ids.ParseWithPrefix(eventID, ids.PrefixEvent)
		require.NoError(t, err)
		assert.Equal(t, int64(1), count(ctx, t, pool, `SELECT count(*) FROM processed_events WHERE event_id = $1 AND consumer = $2`, evUUID, app.ConsumerPaymentEvents))
		assert.Equal(t, int64(1), count(ctx, t, pool, `SELECT count(*) FROM journals WHERE event_id = $1`, evUUID))

		// test-mode 帳戶以 test: 前綴隔離
		assert.Equal(t, int64(1), count(ctx, t, pool, `SELECT count(*) FROM accounts WHERE merchant_id = $1 AND code = 'test:merchant_payable'`, m))
		mbs, err := svc.GetMerchantBalances(ctx, m, "TWD", false)
		require.NoError(t, err)
		require.Len(t, mbs, 1)
		assert.Equal(t, int64(1950), mbs[0].Payable)
		live, err := svc.GetMerchantBalances(ctx, m, "TWD", true)
		require.NoError(t, err)
		assert.Zero(t, live[0].Payable)

		// 不記帳事件：只有 processed_events
		authID := ids.New(ids.PrefixEvent)
		ev2 := &paymentv1.PaymentEvent{
			EventId: authID, EventType: paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_AUTHORIZED,
			OccurredAt: timestamppb.Now(), MerchantId: ids.Format(ids.PrefixMerchant, m), PaymentId: "pay_e2e", Livemode: false,
			Payload: &paymentv1.PaymentEvent_PaymentAuthorized{PaymentAuthorized: &paymentv1.PaymentAuthorized{Provider: "mock"}},
		}
		raw2, err := proto.Marshal(ev2)
		require.NoError(t, err)
		require.NoError(t, svc.HandlePaymentEvent(ctx, eventbus.Record{Value: raw2, Headers: map[string]string{eventbus.HeaderEventID: authID}}))
		authUUID, err := ids.ParseWithPrefix(authID, ids.PrefixEvent)
		require.NoError(t, err)
		assert.Equal(t, int64(1), count(ctx, t, pool, `SELECT count(*) FROM processed_events WHERE event_id = $1`, authUUID))
		assert.Equal(t, int64(0), count(ctx, t, pool, `SELECT count(*) FROM journals WHERE event_id = $1`, authUUID))

		// poison（缺 provider）→ 錯誤且 processed_events 未留下紀錄
		badID := ids.New(ids.PrefixEvent)
		ev3, ok := proto.Clone(ev).(*paymentv1.PaymentEvent)
		require.True(t, ok)
		ev3.EventId = badID
		ev3.GetPaymentCaptured().Provider = ""
		raw3, err := proto.Marshal(ev3)
		require.NoError(t, err)
		err = svc.HandlePaymentEvent(ctx, eventbus.Record{Value: raw3, Headers: map[string]string{eventbus.HeaderEventID: badID}})
		require.ErrorIs(t, err, app.ErrPoisonMessage)
		badUUID, err := ids.ParseWithPrefix(badID, ids.PrefixEvent)
		require.NoError(t, err)
		assert.Equal(t, int64(0), count(ctx, t, pool, `SELECT count(*) FROM processed_events WHERE event_id = $1`, badUUID))
	})

	t.Run("ListJournals / ListAccounts pagination and filters", func(t *testing.T) {
		svc := newService(pool)
		m := uuid.New()
		var paymentIDs []string
		for i := 0; i < 5; i++ {
			ev := captureEvent(m, int64(100*(i+1)), 0)
			paymentIDs = append(paymentIDs, ev.PaymentID)
			_, _, err := svc.PostJournal(ctx, mustTemplate(t, ev))
			require.NoError(t, err)
		}
		page, perr := app.NewPage(2, "")
		require.NoError(t, perr)
		var seen []uuid.UUID
		for {
			js, next, err := svc.ListJournals(ctx, app.JournalFilter{MerchantID: &m}, page)
			require.NoError(t, err)
			for _, j := range js {
				seen = append(seen, j.ID)
				assert.Len(t, j.Entries, 2)
			}
			if next == nil {
				break
			}
			page, err = app.NewPage(2, app.EncodeCursor(next))
			require.NoError(t, err)
		}
		assert.Len(t, seen, 5)
		// 由新到舊、不重複
		uniq := map[uuid.UUID]bool{}
		for _, id := range seen {
			uniq[id] = true
		}
		assert.Len(t, uniq, 5)

		byRef, _, err := svc.ListJournals(ctx, app.JournalFilter{ReferenceType: domain.RefPayment, ReferenceID: paymentIDs[2]}, app.Page{Limit: 10})
		require.NoError(t, err)
		require.Len(t, byRef, 1)
		assert.Equal(t, int64(300), byRef[0].TotalDebit())

		mp, err := NewAccountRepo(pool).GetByKey(ctx, domain.MerchantPayable(m, "TWD", true))
		require.NoError(t, err)
		byAcct, _, err := svc.ListJournals(ctx, app.JournalFilter{AccountID: &mp.ID, Currency: "TWD"}, app.Page{Limit: 10})
		require.NoError(t, err)
		assert.Len(t, byAcct, 5)
		live := true
		byTpl, _, err := svc.ListJournals(ctx, app.JournalFilter{MerchantID: &m, Template: domain.TemplateJCAP, Livemode: &live, SourceType: domain.SourcePaymentEvent}, app.Page{Limit: 10})
		require.NoError(t, err)
		assert.Len(t, byTpl, 5)

		accts, _, err := svc.ListAccounts(ctx, app.AccountFilter{MerchantID: &m}, app.Page{Limit: 10})
		require.NoError(t, err)
		assert.Len(t, accts, 1)
		sys, _, err := svc.ListAccounts(ctx, app.AccountFilter{SystemOnly: true, Kind: domain.KindPSPReceivable, Qualifier: "stripe", Livemode: &live}, app.Page{Limit: 10})
		require.NoError(t, err)
		assert.Len(t, sys, 1)
		assert.Equal(t, "psp_receivable:stripe", sys[0].Key.Code)
	})

	t.Run("CreateAccount is idempotent", func(t *testing.T) {
		svc := newService(pool)
		key := domain.BankCash("ctbc_001", "TWD", true)
		a, existed, err := svc.CreateAccount(ctx, key)
		require.NoError(t, err)
		assert.False(t, existed)
		b, existed, err := svc.CreateAccount(ctx, key)
		require.NoError(t, err)
		assert.True(t, existed)
		assert.Equal(t, a.ID, b.ID)
		assert.Equal(t, int64(1), count(ctx, t, pool, `SELECT count(*) FROM balances WHERE account_id = $1`, a.ID))
	})
}

func mustTemplate(t *testing.T, ev domain.PaymentEvent) *domain.Journal {
	t.Helper()
	j, err := domain.TemplateFor(ev, domain.Policy{})
	require.NoError(t, err)
	return j
}

func captureJournalWithEvent(t *testing.T, m uuid.UUID, eventID uuid.UUID) *domain.Journal {
	t.Helper()
	ev := captureEvent(m, 1000, 33)
	ev.EventID = eventID
	return mustTemplate(t, ev)
}

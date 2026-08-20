package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	ledgerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/ledger/v1"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

func twd(n int64) money.Money { return money.Money{AmountMinor: n, Currency: "TWD"} }

var merchantA = uuid.MustParse("018f3c2a-0000-7000-8000-00000000000a")

func captureJournal(t *testing.T, m uuid.UUID, eventID uuid.UUID) *domain.Journal {
	t.Helper()
	j, err := domain.TemplateFor(domain.PaymentEvent{
		EventID: eventID, EventPublicID: "evt_1", Type: domain.EventPaymentCaptured, OccurredAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		MerchantID: m, PaymentID: "pay_1", Livemode: true, Provider: "stripe", Amount: twd(1000), Fee: twd(33),
	}, domain.Policy{})
	require.NoError(t, err)
	return j
}

func TestPostJournal_CreatesAccountsJournalAndOutbox(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	ctx := context.Background()
	evID := uuid.New()

	j, replayed, err := svc.PostJournal(ctx, captureJournal(t, merchantA, evID))
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.NotEqual(t, uuid.Nil, j.ID)
	assert.Contains(t, j.PublicID, "jrn_")
	assert.Equal(t, f.now, j.PostedAt)
	assert.Equal(t, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), j.EffectiveAt)
	assert.Equal(t, domain.TemplateJCAP, j.Metadata[domain.MetaTemplate])
	assert.Equal(t, "true", j.Metadata[domain.MetaLivemode])
	for _, e := range j.Entries {
		assert.NotEqual(t, uuid.Nil, e.AccountID)
		assert.NotEqual(t, uuid.Nil, e.ID)
	}

	// 三個帳戶 lazy 建立
	assert.Len(t, f.accounts, 3)
	assert.Len(t, f.journals, 1)
	// outbox 事件
	require.Len(t, f.outbox, 1)
	msg := f.outbox[0]
	assert.Equal(t, EventJournalPosted, msg.EventType)
	assert.Equal(t, AggregateJournal, msg.AggregateType)
	assert.Equal(t, MerchantPublicID(merchantA), msg.AggregateID)
	assert.Equal(t, j.PublicID, msg.Headers["journal_id"])
	var pj ledgerv1.Journal
	require.NoError(t, proto.Unmarshal(msg.Payload, &pj))
	assert.Equal(t, j.PublicID, pj.GetId())
	assert.Len(t, pj.GetEntries(), 3)
	assert.Equal(t, ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_PAYMENT_EVENT, pj.GetSourceType())
	assert.Equal(t, "TWD", pj.GetCurrency())

	// 餘額
	assert.Equal(t, int64(1000), f.balances.Of(domain.PSPReceivable("stripe", "TWD", true)))
	assert.Equal(t, int64(967), f.balances.Of(domain.MerchantPayable(merchantA, "TWD", true)))
	assert.Equal(t, int64(33), f.balances.Of(domain.FeeRevenue("TWD", true)))
}

func TestPostJournal_IdempotentOnEventID(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	ctx := context.Background()
	evID := uuid.New()

	first, _, err := svc.PostJournal(ctx, captureJournal(t, merchantA, evID))
	require.NoError(t, err)
	second, replayed, err := svc.PostJournal(ctx, captureJournal(t, merchantA, evID))
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first.ID, second.ID)
	assert.Len(t, f.journals, 1)
	assert.Len(t, f.outbox, 1)
	assert.Equal(t, int64(967), f.balances.Of(domain.MerchantPayable(merchantA, "TWD", true)))
}

func TestPostJournal_DuplicateRaceFallsBackToExisting(t *testing.T) {
	// 模擬 GetByEventID 未命中但 Insert 撞唯一鍵（兩個 consumer 同時寫）。
	svc, f := newTestService(domain.Policy{})
	ctx := context.Background()
	evID := uuid.New()
	first, _, err := svc.PostJournal(ctx, captureJournal(t, merchantA, evID))
	require.NoError(t, err)

	// 讓 repo 的 Insert 回 ErrDuplicateEvent，但 GetByEventID 在交易內先「看不到」：以 failInsert 模擬。
	f.failInsert = domain.ErrDuplicateEvent
	// 需要 GetByEventID 在交易內回 not found：暫時移除 journal，交易失敗後放回。
	saved := f.journals
	f.journals = nil
	j := captureJournal(t, merchantA, evID)
	var got *domain.Journal
	var replayed bool
	errRun := svc.tx.RunInTx(ctx, func(ctx context.Context) error {
		var err error
		got, replayed, err = svc.postJournalTx(ctx, j)
		return err
	})
	require.ErrorIs(t, errRun, domain.ErrDuplicateEvent)
	assert.Nil(t, got)
	assert.False(t, replayed)
	f.journals = saved
	f.failInsert = nil

	// 外層 PostJournal 在 ErrDuplicateEvent 時改讀既有 journal
	f.failInsert = domain.ErrDuplicateEvent
	f.journals = nil
	svcDup := svc
	// 交易失敗後 restore 會把 journals 還原成 nil，因此 GetByEventID 仍查不到 → 回錯誤；
	// 再把 journal 放回後應成功回傳既有 journal。
	_, _, err = svcDup.PostJournal(ctx, captureJournal(t, merchantA, evID))
	require.Error(t, err)
	f.journals = saved
	got, replayed, err = svcDup.PostJournal(ctx, captureJournal(t, merchantA, evID))
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, first.ID, got.ID)
}

func TestPostJournal_RejectsInvalidAndRollsBack(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	ctx := context.Background()

	// 不平衡 → domain 拒絕，沒有任何副作用
	j := captureJournal(t, merchantA, uuid.New())
	j.Entries[1].Amount = twd(900)
	_, _, err := svc.PostJournal(ctx, j)
	assert.ErrorIs(t, err, domain.ErrJournalUnbalanced)
	assert.Empty(t, f.accounts)
	assert.Empty(t, f.outbox)

	// outbox 失敗 → 整筆 rollback（帳戶、journal、餘額皆還原）
	f.failOutbox = errors.New("outbox down")
	_, _, err = svc.PostJournal(ctx, captureJournal(t, merchantA, uuid.New()))
	require.Error(t, err)
	assert.Empty(t, f.accounts)
	assert.Empty(t, f.journals)
	assert.Zero(t, f.balances.Of(domain.MerchantPayable(merchantA, "TWD", true)))
	f.failOutbox = nil

	// 凍結帳戶 → ErrAccountInactive
	_, _, err = svc.PostJournal(ctx, captureJournal(t, merchantA, uuid.New()))
	require.NoError(t, err)
	f.freeze(domain.MerchantPayable(merchantA, "TWD", true))
	_, _, err = svc.PostJournal(ctx, captureJournal(t, merchantA, uuid.New()))
	assert.ErrorIs(t, err, domain.ErrAccountInactive)
	assert.Len(t, f.journals, 1)
}

func TestReverseJournal(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	ctx := context.Background()
	orig, _, err := svc.PostJournal(ctx, captureJournal(t, merchantA, uuid.New()))
	require.NoError(t, err)

	rev, replayed, err := svc.ReverseJournal(ctx, orig.ID, "ops-ticket-42", "wrong fee")
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, domain.RefReversal, rev.ReferenceType)
	require.NotNil(t, rev.ReversalOf)
	assert.Equal(t, orig.ID, *rev.ReversalOf)
	for k, v := range f.balances {
		assert.Zero(t, v, k)
	}
	// 原 journal 讀回時帶 ReversedBy
	got, err := svc.GetJournal(ctx, orig.ID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, got.ReversedBy)
	assert.Equal(t, rev.ID, *got.ReversedBy)

	// 同 idempotency key 重放
	again, replayed, err := svc.ReverseJournal(ctx, orig.ID, "ops-ticket-42", "wrong fee")
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, rev.ID, again.ID)

	// 第二次沖銷（不同 key）被拒
	_, _, err = svc.ReverseJournal(ctx, orig.ID, "ops-ticket-43", "again")
	assert.ErrorIs(t, err, domain.ErrJournalAlreadyReversed)
	assert.Len(t, f.journals, 2)

	// 不存在
	_, _, err = svc.ReverseJournal(ctx, uuid.New(), "k", "")
	assert.ErrorIs(t, err, domain.ErrJournalNotFound)
}

func TestPostManualJournal(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	ctx := context.Background()
	susp, _, err := svc.CreateAccount(ctx, domain.SettlementSuspense("stripe", "TWD", true))
	require.NoError(t, err)
	recv, existed, err := svc.CreateAccount(ctx, domain.PSPReceivable("stripe", "TWD", true))
	require.NoError(t, err)
	assert.False(t, existed)
	_, existed, err = svc.CreateAccount(ctx, domain.PSPReceivable("stripe", "TWD", true))
	require.NoError(t, err)
	assert.True(t, existed)

	in := PostJournalInput{
		IdempotencyKey: "discrepancy-7",
		SourceType:     domain.SourceReconciliationAdjustment,
		SourceID:       "disc_7",
		Description:    "J-STL-DIFF settlement difference",
		Livemode:       true,
		Metadata:       map[string]string{"settlement_id": "stl_9"},
		Entries: []EntryInput{
			{AccountID: susp.ID, Direction: domain.Debit, Amount: twd(12)},
			{AccountID: recv.ID, Direction: domain.Credit, Amount: twd(12)},
		},
	}
	j, replayed, err := svc.PostManualJournal(ctx, in)
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, domain.RefAdjustment, j.ReferenceType)
	assert.Equal(t, "disc_7", j.ReferenceID)
	assert.Equal(t, uuid.Nil, j.MerchantID)
	assert.Equal(t, "stl_9", j.Metadata["settlement_id"])
	assert.Equal(t, int64(12), f.balances.Of(domain.SettlementSuspense("stripe", "TWD", true)))

	// 冪等（任意字串 key → 確定性 uuid）
	j2, replayed, err := svc.PostManualJournal(ctx, in)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, j.ID, j2.ID)

	// 禁止 PAYMENT_EVENT
	bad := in
	bad.IdempotencyKey, bad.SourceType = "x", domain.SourcePaymentEvent
	_, _, err = svc.PostManualJournal(ctx, bad)
	require.Error(t, err)

	// 未知帳戶
	bad = in
	bad.IdempotencyKey = "y"
	bad.Entries = []EntryInput{{AccountID: uuid.New(), Direction: domain.Debit, Amount: twd(1)}, {AccountID: recv.ID, Direction: domain.Credit, Amount: twd(1)}}
	_, _, err = svc.PostManualJournal(ctx, bad)
	assert.ErrorIs(t, err, domain.ErrAccountNotFound)

	// 缺 description / key
	bad = in
	bad.IdempotencyKey, bad.Description = "z", ""
	_, _, err = svc.PostManualJournal(ctx, bad)
	require.Error(t, err)
	bad = in
	bad.IdempotencyKey = ""
	_, _, err = svc.PostManualJournal(ctx, bad)
	assert.ErrorIs(t, err, domain.ErrEventIDMissing)

	// 以 reverses_journal_id 手動沖銷：必須為鏡像
	revIn := PostJournalInput{
		IdempotencyKey: "rev-1", SourceType: domain.SourceReversal, Description: "undo", Livemode: true,
		ReversesJournalID: &j.ID,
		Entries: []EntryInput{
			{AccountID: susp.ID, Direction: domain.Credit, Amount: twd(12)},
			{AccountID: recv.ID, Direction: domain.Debit, Amount: twd(12)},
		},
	}
	rev, _, err := svc.PostManualJournal(ctx, revIn)
	require.NoError(t, err)
	assert.Equal(t, domain.RefReversal, rev.ReferenceType)
	assert.Zero(t, f.balances.Of(domain.SettlementSuspense("stripe", "TWD", true)))

	notMirror := revIn
	notMirror.IdempotencyKey = "rev-2"
	notMirror.Entries[0].Amount = twd(11)
	notMirror.Entries[1].Amount = twd(11)
	_, _, err = svc.PostManualJournal(ctx, notMirror)
	assert.ErrorIs(t, err, domain.ErrJournalAlreadyReversed)

	// reverses_journal_id 但 source_type 不是 REVERSAL
	wrongSrc := revIn
	wrongSrc.IdempotencyKey, wrongSrc.SourceType = "rev-3", domain.SourceManualAdjustment
	_, _, err = svc.PostManualJournal(ctx, wrongSrc)
	require.Error(t, err)
}

func TestQueries(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	ctx := context.Background()
	merchantB := uuid.New()
	for i := 0; i < 3; i++ {
		_, _, err := svc.PostJournal(ctx, captureJournal(t, merchantA, uuid.New()))
		require.NoError(t, err)
	}
	_, _, err := svc.PostJournal(ctx, captureJournal(t, merchantB, uuid.New()))
	require.NoError(t, err)

	// ListJournals 分頁
	page, err := NewPage(2, "")
	require.NoError(t, err)
	js, next, err := svc.ListJournals(ctx, JournalFilter{MerchantID: &merchantA}, page)
	require.NoError(t, err)
	assert.Len(t, js, 2)
	require.NotNil(t, next)
	page2, err := NewPage(2, EncodeCursor(next))
	require.NoError(t, err)
	js2, next2, err := svc.ListJournals(ctx, JournalFilter{MerchantID: &merchantA}, page2)
	require.NoError(t, err)
	assert.Len(t, js2, 1)
	assert.Nil(t, next2)
	_, err = NewPage(2, "not-a-token")
	assert.ErrorIs(t, err, ErrPageTokenInvalid)

	// GetJournal 商戶隔離
	_, err = svc.GetJournal(ctx, js[0].ID, merchantB)
	assert.ErrorIs(t, err, domain.ErrJournalNotFound)
	got, err := svc.GetJournal(ctx, js[0].ID, merchantA)
	require.NoError(t, err)
	assert.Equal(t, js[0].ID, got.ID)

	// ListAccounts
	live := true
	accts, _, err := svc.ListAccounts(ctx, AccountFilter{MerchantID: &merchantA, Livemode: &live}, Page{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, accts, 1)
	sys, _, err := svc.ListAccounts(ctx, AccountFilter{SystemOnly: true}, Page{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, sys, 2)
	psp, _, err := svc.ListAccounts(ctx, AccountFilter{Kind: domain.KindPSPReceivable, Qualifier: "stripe"}, Page{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, psp, 1)

	// GetAccount / GetBalance
	a, err := svc.GetAccount(ctx, accts[0].ID)
	require.NoError(t, err)
	assert.Equal(t, domain.KindMerchantPayable, a.Kind())
	b, err := svc.GetBalance(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(967*3), b.Balance)
	assert.Equal(t, int64(967*3), b.TotalCredit)
	assert.Equal(t, int64(3), b.EntryCount)
	_, err = svc.GetBalance(ctx, uuid.New())
	assert.ErrorIs(t, err, domain.ErrAccountNotFound)

	// GetMerchantBalances
	mbs, err := svc.GetMerchantBalances(ctx, merchantA, "TWD", true)
	require.NoError(t, err)
	require.Len(t, mbs, 1)
	assert.Equal(t, int64(967*3), mbs[0].Payable)
	assert.Equal(t, int64(967*3), mbs[0].Available)
	assert.Zero(t, mbs[0].Pending)
	// 無帳戶的商戶 → 零餘額
	mbs, err = svc.GetMerchantBalances(ctx, uuid.New(), "USD", true)
	require.NoError(t, err)
	require.Len(t, mbs, 1)
	assert.Zero(t, mbs[0].Payable)
	_, err = svc.GetMerchantBalances(ctx, uuid.Nil, "TWD", true)
	require.Error(t, err)
	_, err = svc.GetMerchantBalances(ctx, merchantA, "ZZZ", true)
	assert.ErrorIs(t, err, domain.ErrInvalidCurrency)
	_ = f
}

func TestIdempotencyKeyToEventID(t *testing.T) {
	u := uuid.New()
	got, err := IdempotencyKeyToEventID(u.String())
	require.NoError(t, err)
	assert.Equal(t, u, got)

	evt := "evt_" + "01J5X1Y2Z3A4B5C6D7E8F9G0H1"
	got, err = IdempotencyKeyToEventID(evt)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, got)

	a, _ := IdempotencyKeyToEventID("ticket-1")
	b, _ := IdempotencyKeyToEventID("ticket-1")
	c, _ := IdempotencyKeyToEventID("ticket-2")
	assert.Equal(t, a, b)
	assert.NotEqual(t, a, c)

	_, err = IdempotencyKeyToEventID("  ")
	assert.ErrorIs(t, err, domain.ErrEventIDMissing)
}

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{At: time.Date(2026, 8, 20, 1, 2, 3, 4, time.UTC), ID: uuid.New()}
	got, err := DecodeCursor(EncodeCursor(&c))
	require.NoError(t, err)
	assert.Equal(t, c, got)
	assert.Empty(t, EncodeCursor(nil))
	p, err := NewPage(0, "")
	require.NoError(t, err)
	assert.Equal(t, DefaultPageSize, p.Limit)
	p, err = NewPage(1000, "")
	require.NoError(t, err)
	assert.Equal(t, MaxPageSize, p.Limit)
}

package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

var (
	testMerchant = uuid.MustParse("018f3c2a-0000-7000-8000-000000000001")
	testNow      = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
)

func baseEvent(typ EventType) PaymentEvent {
	return PaymentEvent{
		EventID:          uuid.New(),
		EventPublicID:    "evt_test",
		Type:             typ,
		OccurredAt:       testNow,
		MerchantID:       testMerchant,
		MerchantPublicID: "mch_test",
		PaymentID:        "pay_1",
		Livemode:         true,
		Provider:         "stripe",
		Amount:           twd(1000),
		Fee:              twd(33),
	}
}

// line 為分錄的可比較表示（測試用）。
type line struct {
	kind Kind
	dir  Direction
	amt  int64
}

func lines(j *Journal) []line {
	out := make([]line, 0, len(j.Entries))
	for _, e := range j.Entries {
		out = append(out, line{e.Account.Kind(), e.Direction, e.Amount.AmountMinor})
	}
	return out
}

func TestTemplateFor_Mapping(t *testing.T) {
	m := testMerchant
	tests := []struct {
		name         string
		ev           func() PaymentEvent
		policy       Policy
		wantTemplate string
		wantRef      ReferenceType
		wantRefID    string
		wantLines    []line
	}{
		{
			name: "J-CAP payment.captured", ev: func() PaymentEvent { return baseEvent(EventPaymentCaptured) },
			wantTemplate: TemplateJCAP, wantRef: RefPayment, wantRefID: "pay_1",
			wantLines: []line{{KindPSPReceivable, Debit, 1000}, {KindMerchantPayable, Credit, 967}, {KindFeeRevenue, Credit, 33}},
		},
		{
			name: "J-CAP without fee", ev: func() PaymentEvent { e := baseEvent(EventPaymentCaptured); e.Fee = money.Money{}; return e },
			wantTemplate: TemplateJCAP, wantRef: RefPayment, wantRefID: "pay_1",
			wantLines: []line{{KindPSPReceivable, Debit, 1000}, {KindMerchantPayable, Credit, 1000}},
		},
		{
			name: "J-CAP fee equals amount", ev: func() PaymentEvent { e := baseEvent(EventPaymentCaptured); e.Fee = twd(1000); return e },
			wantTemplate: TemplateJCAP, wantRef: RefPayment, wantRefID: "pay_1",
			wantLines: []line{{KindPSPReceivable, Debit, 1000}, {KindFeeRevenue, Credit, 1000}},
		},
		{
			name: "J-REF-PEND refund.created", ev: func() PaymentEvent {
				e := baseEvent(EventRefundCreated)
				e.RefundID, e.Amount, e.Fee = "re_1", twd(300), money.Money{}
				return e
			},
			wantTemplate: TemplateJREFPEND, wantRef: RefRefund, wantRefID: "re_1",
			wantLines: []line{{KindMerchantPayable, Debit, 300}, {KindRefundClearing, Credit, 300}},
		},
		{
			name: "J-REF-OK refund.succeeded", ev: func() PaymentEvent {
				e := baseEvent(EventRefundSucceeded)
				e.RefundID, e.Amount, e.Fee = "re_1", twd(300), money.Money{}
				return e
			},
			wantTemplate: TemplateJREFOK, wantRef: RefRefund, wantRefID: "re_1",
			wantLines: []line{{KindRefundClearing, Debit, 300}, {KindPSPReceivable, Credit, 300}},
		},
		{
			name: "J-REF-OK + J-REF-FEE refund.succeeded with fee", ev: func() PaymentEvent {
				e := baseEvent(EventRefundSucceeded)
				e.RefundID, e.Amount, e.Fee = "re_1", twd(300), twd(5)
				return e
			},
			wantTemplate: TemplateJREFOK, wantRef: RefRefund, wantRefID: "re_1",
			wantLines: []line{{KindRefundClearing, Debit, 300}, {KindPSPReceivable, Credit, 300}, {KindMerchantPayable, Debit, 5}, {KindFeeRevenue, Credit, 5}},
		},
		{
			name: "J-REF-FAIL refund.failed", ev: func() PaymentEvent {
				e := baseEvent(EventRefundFailed)
				e.RefundID, e.Amount, e.Fee = "re_1", twd(300), money.Money{}
				return e
			},
			wantTemplate: TemplateJREFFail, wantRef: RefRefund, wantRefID: "re_1",
			wantLines: []line{{KindRefundClearing, Debit, 300}, {KindMerchantPayable, Credit, 300}},
		},
		{
			name: "J-CB-OPEN dispute.opened with fee", ev: func() PaymentEvent {
				e := baseEvent(EventDisputeOpened)
				e.DisputeID, e.Fee = "dp_1", twd(450)
				return e
			},
			wantTemplate: TemplateJCBOpen, wantRef: RefDispute, wantRefID: "dp_1",
			wantLines: []line{{KindMerchantPayable, Debit, 1000}, {KindChargebackReserve, Credit, 1000}, {KindMerchantPayable, Debit, 450}, {KindChargebackFeeRevenue, Credit, 450}},
		},
		{
			name: "J-CB-LOST dispute.lost", ev: func() PaymentEvent {
				e := baseEvent(EventDisputeLost)
				e.DisputeID, e.Fee = "dp_1", twd(450)
				return e
			},
			wantTemplate: TemplateJCBLost, wantRef: RefDispute, wantRefID: "dp_1",
			wantLines: []line{{KindChargebackReserve, Debit, 1000}, {KindPSPReceivable, Credit, 1000}},
		},
		{
			name: "J-CB-WON dispute.won (fee kept by default)", ev: func() PaymentEvent {
				e := baseEvent(EventDisputeWon)
				e.DisputeID, e.Fee = "dp_1", twd(450)
				return e
			},
			wantTemplate: TemplateJCBWon, wantRef: RefDispute, wantRefID: "dp_1",
			wantLines: []line{{KindChargebackReserve, Debit, 1000}, {KindMerchantPayable, Credit, 1000}},
		},
		{
			name: "J-CB-WON + J-CB-WON-FEE", ev: func() PaymentEvent {
				e := baseEvent(EventDisputeWon)
				e.DisputeID, e.Fee = "dp_1", twd(450)
				return e
			},
			policy:       Policy{RefundChargebackFeeOnWin: true},
			wantTemplate: TemplateJCBWon, wantRef: RefDispute, wantRefID: "dp_1",
			wantLines: []line{{KindChargebackReserve, Debit, 1000}, {KindMerchantPayable, Credit, 1000}, {KindChargebackFeeRevenue, Debit, 450}, {KindMerchantPayable, Credit, 450}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := tt.ev()
			j, err := TemplateFor(ev, tt.policy)
			require.NoError(t, err)
			require.NoError(t, j.Validate())
			assert.Equal(t, tt.wantTemplate, j.Template)
			assert.Equal(t, tt.wantRef, j.ReferenceType)
			assert.Equal(t, tt.wantRefID, j.ReferenceID)
			assert.Equal(t, ev.EventID, j.EventID)
			assert.Equal(t, m, j.MerchantID)
			assert.Equal(t, SourcePaymentEvent, j.SourceType)
			assert.Equal(t, "evt_test", j.SourceID)
			assert.Equal(t, "pay_1", j.Metadata[MetaPaymentID])
			assert.Equal(t, testNow, j.EffectiveAt)
			assert.Equal(t, tt.wantLines, lines(j))
			for _, e := range j.Entries {
				assert.True(t, e.Account.Livemode)
				if !e.Account.IsSystem() {
					assert.Equal(t, m, e.Account.MerchantID)
				}
			}
		})
	}
}

func TestTemplateFor_NoTemplate(t *testing.T) {
	for _, typ := range []EventType{
		EventPaymentCreated, EventPaymentRequiresAction, EventPaymentAuthorized,
		EventPaymentVoided, EventPaymentFailed, EventPaymentExpired, EventDisputeEvidenceSubmitted, "something.else",
	} {
		_, err := TemplateFor(baseEvent(typ), Policy{})
		assert.ErrorIs(t, err, ErrNoTemplate, typ)
	}
}

func TestTemplateFor_InvalidEvents(t *testing.T) {
	tests := []struct {
		name    string
		ev      func() PaymentEvent
		wantErr error
	}{
		{"fee exceeds amount", func() PaymentEvent { e := baseEvent(EventPaymentCaptured); e.Fee = twd(1001); return e }, ErrFeeExceedsAmount},
		{"missing provider on capture", func() PaymentEvent { e := baseEvent(EventPaymentCaptured); e.Provider = ""; return e }, ErrEventInvalid},
		{"bad provider qualifier", func() PaymentEvent { e := baseEvent(EventPaymentCaptured); e.Provider = "Stripe!"; return e }, ErrEventInvalid},
		{"missing merchant", func() PaymentEvent { e := baseEvent(EventPaymentCaptured); e.MerchantID = uuid.Nil; return e }, ErrEventInvalid},
		{"missing event id", func() PaymentEvent { e := baseEvent(EventPaymentCaptured); e.EventID = uuid.Nil; return e }, ErrEventInvalid},
		{"zero amount", func() PaymentEvent {
			e := baseEvent(EventPaymentCaptured)
			e.Amount = twd(0)
			e.Fee = money.Money{}
			return e
		}, ErrEventInvalid},
		{"bad currency", func() PaymentEvent {
			e := baseEvent(EventPaymentCaptured)
			e.Amount = money.Money{AmountMinor: 1, Currency: "ZZZ"}
			e.Fee = money.Money{}
			return e
		}, ErrEventInvalid},
		{"fee currency differs", func() PaymentEvent { e := baseEvent(EventPaymentCaptured); e.Fee = money.MustNew(1, "USD"); return e }, ErrEventInvalid},
		{"refund without refund id", func() PaymentEvent { e := baseEvent(EventRefundSucceeded); return e }, ErrEventInvalid},
		{"dispute without dispute id", func() PaymentEvent { e := baseEvent(EventDisputeOpened); return e }, ErrEventInvalid},
		{"dispute.lost without provider", func() PaymentEvent {
			e := baseEvent(EventDisputeLost)
			e.DisputeID, e.Provider = "dp_1", ""
			return e
		}, ErrEventInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := TemplateFor(tt.ev(), Policy{})
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestTemplateFor_TestModeUsesTestAccounts(t *testing.T) {
	ev := baseEvent(EventPaymentCaptured)
	ev.Livemode = false
	j, err := TemplateFor(ev, Policy{})
	require.NoError(t, err)
	assert.False(t, j.Livemode)
	for _, e := range j.Entries {
		assert.False(t, e.Account.Livemode)
		assert.Equal(t, "test:"+e.Account.Code, StorageCode(e.Account.Code, e.Account.Livemode))
	}
}

func TestSettlementTemplate(t *testing.T) {
	s := SettlementPosted{
		EventID: uuid.New(), SettlementID: "stl_1", Provider: "stripe", BankAccount: "ctbc_001", Livemode: true, OccurredAt: testNow,
		Gross: twd(1000), PSPFees: twd(25), NetPaid: twd(975),
	}
	j, err := SettlementTemplate(s)
	require.NoError(t, err)
	assert.Equal(t, TemplateJSTL, j.Template)
	assert.Equal(t, RefSettlement, j.ReferenceType)
	assert.Equal(t, uuid.Nil, j.MerchantID)
	assert.Equal(t, []line{{KindBankCash, Debit, 975}, {KindPSPFeeExpense, Debit, 25}, {KindPSPReceivable, Credit, 1000}}, lines(j))

	// gross ≠ net + fees → 必須走 J-STL-DIFF
	bad := s
	bad.NetPaid = twd(900)
	_, err = SettlementTemplate(bad)
	assert.ErrorIs(t, err, ErrJournalUnbalanced)

	// 缺欄位
	bad = s
	bad.BankAccount = ""
	_, err = SettlementTemplate(bad)
	assert.ErrorIs(t, err, ErrEventInvalid)
}

// TestExampleTrace 重現 docs/02 §7.3 的範例追蹤：1,000 付款 → 300 退款 → 結算 700。
func TestExampleTrace(t *testing.T) {
	bal := Balances{}
	apply := func(ev PaymentEvent) {
		j, err := TemplateFor(ev, Policy{})
		require.NoError(t, err)
		require.NoError(t, bal.Apply(j))
	}
	apply(baseEvent(EventPaymentCaptured)) // J-CAP 1000 / 967 / 33

	ref := baseEvent(EventRefundCreated)
	ref.RefundID, ref.Amount, ref.Fee = "re_1", twd(300), money.Money{}
	apply(ref) // J-REF-PEND
	ref.Type = EventRefundSucceeded
	ref.EventID = uuid.New()
	apply(ref) // J-REF-OK

	stl, err := SettlementTemplate(SettlementPosted{
		EventID: uuid.New(), SettlementID: "stl_1", Provider: "stripe", BankAccount: "ctbc_001", Livemode: true, OccurredAt: testNow,
		Gross: twd(700), PSPFees: twd(25), NetPaid: twd(675),
	})
	require.NoError(t, err)
	require.NoError(t, bal.Apply(stl))

	m := testMerchant
	assert.Equal(t, int64(0), bal.Of(PSPReceivable("stripe", "TWD", true)))
	assert.Equal(t, int64(667), bal.Of(MerchantPayable(m, "TWD", true)))
	assert.Equal(t, int64(0), bal.Of(RefundClearing(m, "TWD", true)))
	assert.Equal(t, int64(33), bal.Of(FeeRevenue("TWD", true)))
	assert.Equal(t, int64(675), bal.Of(BankCash("ctbc_001", "TWD", true)))
	assert.Equal(t, int64(25), bal.Of(PSPFeeExpense("stripe", "TWD", true)))

	tb := bal.TrialBalances()["TWD/true"]
	assert.Equal(t, int64(675), tb.Assets)
	assert.Equal(t, int64(667), tb.Liabilities)
	assert.Equal(t, int64(8), tb.Equity())
	require.NoError(t, bal.CheckIdentity())
}

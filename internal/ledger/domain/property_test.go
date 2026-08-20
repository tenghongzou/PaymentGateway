package domain

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// 屬性測試（docs/09 §4）：以固定 seed 的生成器產生合法的付款生命週期事件序列，
// 驗證 P1（借貸平衡）、P2（全系統借貸總和為 0 / 會計恆等式）、P3（merchant_payable 恆等式）、
// P4（fee_revenue = Σfee 且 net + fee = gross）、P5（沖銷回到原點）、P7（交錯無關性）。
//
// 迭代次數：預設 1000；可用 LEDGER_PROP_ITER 覆寫（nightly 10000）。seed 由 LEDGER_PROP_SEED 指定或隨機並印出。

func propIterations(t *testing.T) int {
	t.Helper()
	if v := os.Getenv("LEDGER_PROP_ITER"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err)
		return n
	}
	if testing.Short() {
		return 100
	}
	return 1000
}

func propRand(t *testing.T) *rand.Rand {
	t.Helper()
	seed := time.Now().UnixNano()
	if v := os.Getenv("LEDGER_PROP_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		require.NoError(t, err)
		seed = n
	}
	t.Logf("property seed: %d (reproduce with LEDGER_PROP_SEED=%d)", seed, seed)
	return rand.New(rand.NewSource(seed)) //nolint:gosec // 測試用、可重現
}

var propCurrencies = []string{"TWD", "USD", "JPY", "KWD"}
var propProviders = []string{"stripe", "mock", "adyen"}

// genMoney 依幣別 exponent 生成金額，混合邊界值（1、10^exp 的倍數與非倍數、大值）。
func genMoney(r *rand.Rand, currency string, maxMinor int64) money.Money {
	if maxMinor < 1 {
		maxMinor = 1
	}
	var amt int64
	switch r.Intn(6) {
	case 0:
		amt = 1
	case 1:
		amt = maxMinor
	case 2:
		unit := int64(1)
		for range money.Exponent(currency) {
			unit *= 10
		}
		amt = unit * (1 + r.Int63n(max(maxMinor/unit, 1)))
	default:
		amt = 1 + r.Int63n(maxMinor)
	}
	if amt > maxMinor {
		amt = maxMinor
	}
	return money.Money{AmountMinor: amt, Currency: currency}
}

// lifecycle 為一個 payment 的事件序列與預期值。
type lifecycle struct {
	paymentID string
	merchant  uuid.UUID
	currency  string
	livemode  bool
	events    []PaymentEvent

	captured     int64
	fee          int64
	refunded     int64 // 成功退款合計（含 pending 尚未成功者 → 見 pending）
	refundPend   int64 // 已 J-REF-PEND 但尚未 OK / FAIL
	refundFees   int64
	cbOpened     int64
	cbFee        int64
	cbWon        int64
	cbLost       int64
	cbFeeRefund  int64
	policyRefund bool
}

// genPaymentLifecycle：captured → 0..n 個 partial refund（pending → succeeded | failed | 留在 pending）→ 可選 dispute → won | lost | open。
func genPaymentLifecycle(r *rand.Rand, merchant uuid.UUID, currency string, livemode bool, pol Policy) lifecycle {
	lc := lifecycle{paymentID: "pay_" + uuid.NewString()[:8], merchant: merchant, currency: currency, livemode: livemode, policyRefund: pol.RefundChargebackFeeOnWin}
	maxAmt := int64(1_000_000)
	if r.Intn(10) == 0 {
		maxAmt = 1_000_000_000_000 // 10^12 邊界
	}
	amount := genMoney(r, currency, maxAmt)
	fee := money.Zero(currency)
	if r.Intn(4) != 0 {
		fc, _ := FeePolicy{FixedMinor: r.Int63n(50), PercentageBps: r.Int63n(500)}.Calculate(amount)
		fee = fc.Total
	}
	provider := propProviders[r.Intn(len(propProviders))]
	mk := func(typ EventType) PaymentEvent {
		return PaymentEvent{
			EventID: uuid.New(), EventPublicID: "evt_" + uuid.NewString()[:8], Type: typ, OccurredAt: testNow,
			MerchantID: merchant, PaymentID: lc.paymentID, Livemode: livemode, Provider: provider,
		}
	}
	// 不記帳的前置事件也混進序列
	if r.Intn(2) == 0 {
		e := mk(EventPaymentCreated)
		e.Amount = amount
		lc.events = append(lc.events, e)
	}
	cap := mk(EventPaymentCaptured)
	cap.Amount, cap.Fee = amount, fee
	lc.events = append(lc.events, cap)
	lc.captured, lc.fee = amount.AmountMinor, fee.AmountMinor

	// 退款：總和 ≤ captured
	remaining := amount.AmountMinor
	for i := 0; i < r.Intn(4) && remaining > 0; i++ {
		ra := genMoney(r, currency, remaining)
		remaining -= ra.AmountMinor
		refundID := fmt.Sprintf("re_%s_%d", lc.paymentID, i)
		pend := mk(EventRefundCreated)
		pend.RefundID, pend.Amount = refundID, ra
		lc.events = append(lc.events, pend)
		switch r.Intn(3) {
		case 0: // 成功
			ok := mk(EventRefundSucceeded)
			ok.RefundID, ok.Amount = refundID, ra
			if r.Intn(3) == 0 {
				ok.Fee = money.Money{AmountMinor: 1 + r.Int63n(10), Currency: currency}
				lc.refundFees += ok.Fee.AmountMinor
			}
			lc.events = append(lc.events, ok)
			lc.refunded += ra.AmountMinor
		case 1: // 失敗 → 沖回
			fail := mk(EventRefundFailed)
			fail.RefundID, fail.Amount = refundID, ra
			lc.events = append(lc.events, fail)
		default: // 留在 pending
			lc.refundPend += ra.AmountMinor
		}
	}

	// 爭議
	if r.Intn(3) == 0 {
		da := genMoney(r, currency, amount.AmountMinor)
		dfee := money.Zero(currency)
		if r.Intn(2) == 0 {
			dfee = money.Money{AmountMinor: 1 + r.Int63n(500), Currency: currency}
		}
		disputeID := "dp_" + lc.paymentID
		open := mk(EventDisputeOpened)
		open.DisputeID, open.Amount, open.Fee = disputeID, da, dfee
		lc.events = append(lc.events, open)
		lc.cbOpened, lc.cbFee = da.AmountMinor, dfee.AmountMinor
		switch r.Intn(3) {
		case 0:
			won := mk(EventDisputeWon)
			won.DisputeID, won.Amount, won.Fee = disputeID, da, dfee
			lc.events = append(lc.events, won)
			lc.cbWon = da.AmountMinor
			if pol.RefundChargebackFeeOnWin {
				lc.cbFeeRefund = dfee.AmountMinor
			}
		case 1:
			lost := mk(EventDisputeLost)
			lost.DisputeID, lost.Amount, lost.Fee = disputeID, da, dfee
			lc.events = append(lc.events, lost)
			lc.cbLost = da.AmountMinor
		}
	}
	return lc
}

// expectedMerchantPayable 依生命週期推導 merchant_payable 應有餘額（P3）。
func (lc lifecycle) expectedMerchantPayable() int64 {
	return lc.captured - lc.fee - lc.refunded - lc.refundPend - lc.refundFees - lc.cbOpened - lc.cbFee + lc.cbWon + lc.cbFeeRefund
}

// applyAll 逐一記帳；回傳 journals。
func applyAll(t *testing.T, bal Balances, events []PaymentEvent, pol Policy) []*Journal {
	t.Helper()
	var out []*Journal
	for _, ev := range events {
		j, err := TemplateFor(ev, pol)
		if err != nil {
			require.ErrorIs(t, err, ErrNoTemplate, "event %s", ev.Type)
			continue
		}
		// P1：每筆被接受的 journal 借貸相等
		require.NoError(t, j.Validate())
		require.Equal(t, j.TotalDebit(), j.TotalCredit(), "P1 violated for %s", j.Template)
		require.NoError(t, bal.Apply(j))
		out = append(out, j)
	}
	return out
}

func TestProperty_LifecycleInvariants(t *testing.T) {
	r := propRand(t)
	iters := propIterations(t)
	for i := 0; i < iters; i++ {
		pol := Policy{RefundChargebackFeeOnWin: r.Intn(2) == 0}
		currency := propCurrencies[r.Intn(len(propCurrencies))]
		livemode := r.Intn(5) != 0
		merchant := uuid.New()
		bal := Balances{}

		// 同一商戶 1..3 個 payment
		var lcs []lifecycle
		var totalFee, totalRefundFee, totalCBFee, totalCBFeeRefund int64
		for range 1 + r.Intn(3) {
			lc := genPaymentLifecycle(r, merchant, currency, livemode, pol)
			lcs = append(lcs, lc)
			applyAll(t, bal, lc.events, pol)
			totalFee += lc.fee
			totalRefundFee += lc.refundFees
			totalCBFee += lc.cbFee
			totalCBFeeRefund += lc.cbFeeRefund
		}

		// P2：會計恆等式（Σassets = Σliabilities + Σrevenue − Σexpense）
		require.NoError(t, bal.CheckIdentity(), "iteration %d", i)

		// P3：merchant_payable 餘額
		var expectPayable int64
		for _, lc := range lcs {
			expectPayable += lc.expectedMerchantPayable()
		}
		require.Equal(t, expectPayable, bal.Of(MerchantPayable(merchant, currency, livemode)), "P3 violated at iteration %d", i)

		// P4：fee_revenue = Σ平台費 + Σ退款費；chargeback_fee_revenue = Σ拒付費 − Σ退回
		require.Equal(t, totalFee+totalRefundFee, bal.Of(FeeRevenue(currency, livemode)), "P4 fee_revenue at iteration %d", i)
		require.Equal(t, totalCBFee-totalCBFeeRefund, bal.Of(ChargebackFeeRevenue(currency, livemode)), "P4 chargeback_fee_revenue at iteration %d", i)

		// 負債類不會因範本本身出現負值（refund_clearing / chargeback_reserve ≥ 0）
		require.GreaterOrEqual(t, bal.Of(RefundClearing(merchant, currency, livemode)), int64(0))
		require.GreaterOrEqual(t, bal.Of(ChargebackReserve(merchant, currency, livemode)), int64(0))
	}
}

// P4 補充：J-CAP 的 net + fee == gross，對隨機金額 / 費率成立，且 half-up 不掉分。
func TestProperty_CaptureNetPlusFeeEqualsGross(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIterations(t); i++ {
		currency := propCurrencies[r.Intn(len(propCurrencies))]
		ev := baseEvent(EventPaymentCaptured)
		ev.Amount = genMoney(r, currency, 1_000_000_000)
		fc, err := FeePolicy{FixedMinor: r.Int63n(100), PercentageBps: r.Int63n(1000), MaxFeeMinor: r.Int63n(100000)}.Calculate(ev.Amount)
		require.NoError(t, err)
		ev.Fee = fc.Total
		j, err := TemplateFor(ev, Policy{})
		require.NoError(t, err)
		var gross, net, fee int64
		for _, e := range j.Entries {
			switch e.Account.Kind() {
			case KindPSPReceivable:
				gross += e.Amount.AmountMinor
			case KindMerchantPayable:
				net += e.Amount.AmountMinor
			case KindFeeRevenue:
				fee += e.Amount.AmountMinor
			}
		}
		require.Equal(t, gross, net+fee, "iteration %d", i)
		require.Equal(t, ev.Fee.AmountMinor, fee)
	}
}

// P5：沖銷任一 journal 後餘額回到沖銷前；沖銷再重記等價於直接記正確值。
func TestProperty_ReversalRestoresBalances(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIterations(t); i++ {
		currency := propCurrencies[r.Intn(len(propCurrencies))]
		lc := genPaymentLifecycle(r, uuid.New(), currency, true, Policy{})
		bal := Balances{}
		journals := applyAll(t, bal, lc.events, Policy{})
		require.NotEmpty(t, journals)

		victim := journals[r.Intn(len(journals))]
		victim.ID = uuid.New()
		victim.PublicID = "jrn_" + victim.ID.String()[:8]
		before := bal.Clone()

		rev, err := Reverse(victim, uuid.New(), "", testNow)
		require.NoError(t, err)
		require.NoError(t, bal.Apply(rev))
		// 套用原 journal 再沖銷 = 沖銷前減去原 journal 的影響
		expect := before.Clone()
		for _, e := range victim.Entries {
			spec := chart[e.Account.Kind()]
			expect[e.Account] -= signedDelta(spec.Type.NormalBalance(), e.Direction, e.Amount.AmountMinor)
		}
		require.True(t, bal.Equal(expect), "P5 violated at iteration %d", i)
		require.NoError(t, bal.CheckIdentity())

		// 重記原 journal（等價於直接記正確值）
		require.NoError(t, bal.Apply(victim))
		require.True(t, bal.Equal(before), "P5 re-post violated at iteration %d", i)
	}
}

// P7：跨 payment 任意交錯（同一 payment 內保序）最終餘額相同。
func TestProperty_InterleavingIndependence(t *testing.T) {
	r := propRand(t)
	for i := 0; i < propIterations(t)/2; i++ {
		currency := propCurrencies[r.Intn(len(propCurrencies))]
		merchant := uuid.New()
		pol := Policy{RefundChargebackFeeOnWin: r.Intn(2) == 0}
		n := 2 + r.Intn(3)
		lcs := make([]lifecycle, n)
		for k := range lcs {
			lcs[k] = genPaymentLifecycle(r, merchant, currency, true, pol)
		}
		// 順序 1：逐 payment
		seq := Balances{}
		for _, lc := range lcs {
			applyAll(t, seq, lc.events, pol)
		}
		// 順序 2：隨機交錯（各 payment 內保序）
		idx := make([]int, n)
		var mixed []PaymentEvent
		for {
			var candidates []int
			for k := range lcs {
				if idx[k] < len(lcs[k].events) {
					candidates = append(candidates, k)
				}
			}
			if len(candidates) == 0 {
				break
			}
			k := candidates[r.Intn(len(candidates))]
			mixed = append(mixed, lcs[k].events[idx[k]])
			idx[k]++
		}
		inter := Balances{}
		applyAll(t, inter, mixed, pol)
		require.True(t, seq.Equal(inter), "P7 violated at iteration %d", i)
	}
}

// genJournal：2–8 條 entry 的任意 journal，平衡與不平衡各半；P1 的拒絕面。
func TestProperty_RandomJournalsAcceptedIffBalanced(t *testing.T) {
	r := propRand(t)
	merchant := uuid.New()
	accounts := []AccountKey{
		PSPReceivable("stripe", "TWD", true), BankCash("ctbc", "TWD", true), MerchantPayable(merchant, "TWD", true),
		RefundClearing(merchant, "TWD", true), FeeRevenue("TWD", true), PSPFeeExpense("stripe", "TWD", true),
	}
	for i := 0; i < propIterations(t); i++ {
		n := 2 + r.Intn(7)
		j := &Journal{EventID: uuid.New(), MerchantID: merchant, Livemode: true, ReferenceType: RefAdjustment, ReferenceID: "x"}
		var debits, credits int64
		for k := 0; k < n-1; k++ {
			dir := Debit
			if r.Intn(2) == 0 {
				dir = Credit
			}
			amt := 1 + r.Int63n(10000)
			if dir == Debit {
				debits += amt
			} else {
				credits += amt
			}
			j.Entries = append(j.Entries, Entry{Account: accounts[r.Intn(len(accounts))], Direction: dir, Amount: twd(amt)})
		}
		// 最後一筆決定平衡與否
		balanced := r.Intn(2) == 0
		diff := debits - credits
		last := Entry{Account: accounts[r.Intn(len(accounts))], Amount: twd(1)}
		switch {
		case balanced && diff > 0:
			last.Direction, last.Amount = Credit, twd(diff)
		case balanced && diff < 0:
			last.Direction, last.Amount = Debit, twd(-diff)
		case balanced: // diff == 0：加一對不可能，改成不平衡
			balanced = false
			last.Direction = Debit
		default:
			last.Direction = Debit
			last.Amount = twd(1 + r.Int63n(10000))
			if debits+last.Amount.AmountMinor == credits {
				last.Amount.AmountMinor++
			}
		}
		j.Entries = append(j.Entries, last)
		err := j.Validate()
		if balanced {
			require.NoError(t, err, "iteration %d", i)
		} else {
			require.ErrorIs(t, err, ErrJournalUnbalanced, "iteration %d", i)
		}
	}
}

package domain

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// 分錄範本 ID（docs/02 §7.3）。
const (
	TemplateJCAP      = "J-CAP"        // payment.captured
	TemplateJREFPEND  = "J-REF-PEND"   // refund.created（refund.pending）
	TemplateJREFOK    = "J-REF-OK"     // refund.succeeded（含 J-REF-FEE 行）
	TemplateJREFFEE   = "J-REF-FEE"    // refund.succeeded 且 fee > 0（併入 J-REF-OK 同一 journal）
	TemplateJREFFail  = "J-REF-FAIL"   // refund.failed（沖回 J-REF-PEND）
	TemplateJCBOpen   = "J-CB-OPEN"    // dispute.opened
	TemplateJCBLost   = "J-CB-LOST"    // dispute.lost
	TemplateJCBWon    = "J-CB-WON"     // dispute.won（含 J-CB-WON-FEE 行）
	TemplateJCBWonFee = "J-CB-WON-FEE" // dispute.won 且政策退回拒付費
	TemplateJSTL      = "J-STL"        // settlement.posted
	TemplateJREV      = "J-REV"        // 人工沖銷
)

// TemplateFor 依 docs/02 §7.3 把付款事件映射成分錄。不需記帳的事件回 ErrNoTemplate。
//
// 一個事件對應一筆 journal（journals.event_id 唯一），因此 §7.3 中同一事件觸發的多個範本行
// （J-REF-OK + J-REF-FEE、J-CB-WON + J-CB-WON-FEE、J-CB-OPEN 的金額 + 拒付費）併入同一筆 journal。
func TemplateFor(ev PaymentEvent, pol Policy) (*Journal, error) {
	switch ev.Type {
	case EventPaymentCaptured:
		return templateCapture(ev)
	case EventRefundCreated:
		return templateRefundPending(ev)
	case EventRefundSucceeded:
		return templateRefundSucceeded(ev)
	case EventRefundFailed:
		return templateRefundFailed(ev)
	case EventDisputeOpened:
		return templateDisputeOpened(ev)
	case EventDisputeLost:
		return templateDisputeLost(ev)
	case EventDisputeWon:
		return templateDisputeWon(ev, pol)
	case EventPaymentCreated, EventPaymentRequiresAction, EventPaymentAuthorized,
		EventPaymentVoided, EventPaymentFailed, EventPaymentExpired, EventDisputeEvidenceSubmitted:
		// 授權 / 取消 / 失敗 / 證據提交不是資金移動：不記帳。
		return nil, ErrNoTemplate
	default:
		return nil, fmt.Errorf("%w: %q", ErrNoTemplate, ev.Type)
	}
}

// baseJournal 建立帶共同欄位的 journal。
func baseJournal(ev PaymentEvent, template string, ref ReferenceType, refID, description string) (*Journal, error) {
	if ev.EventID == uuid.Nil {
		return nil, ErrEventInvalid.WithMessage("event_id is required")
	}
	if ev.MerchantID == uuid.Nil {
		return nil, ErrEventInvalid.WithMessage("merchant_id is required")
	}
	if ev.PaymentID == "" {
		return nil, ErrEventInvalid.WithMessage("payment_id is required")
	}
	if err := ev.Amount.Validate(); err != nil {
		return nil, ErrEventInvalid.WithMessage("amount: %v", err)
	}
	if !ev.Amount.IsPositive() {
		return nil, ErrEventInvalid.WithMessage("amount must be > 0")
	}
	if ev.Fee.AmountMinor < 0 {
		return nil, ErrEventInvalid.WithMessage("fee must not be negative")
	}
	if ev.Fee.AmountMinor > 0 && ev.Fee.Currency != ev.Amount.Currency {
		return nil, ErrEventInvalid.WithMessage("fee currency %s differs from amount currency %s", ev.Fee.Currency, ev.Amount.Currency)
	}
	j := &Journal{
		EventID:       ev.EventID,
		MerchantID:    ev.MerchantID,
		Livemode:      ev.Livemode,
		SourceType:    SourcePaymentEvent,
		SourceID:      ev.EventPublicID,
		ReferenceType: ref,
		ReferenceID:   refID,
		Description:   description,
		Template:      template,
		EffectiveAt:   ev.OccurredAt,
		PostedAt:      ev.OccurredAt,
		Metadata: map[string]string{
			MetaPaymentID: ev.PaymentID,
		},
	}
	if ev.Provider != "" {
		j.Metadata[MetaProvider] = ev.Provider
	}
	return j, nil
}

func requireProvider(ev PaymentEvent) error {
	if ev.Provider == "" {
		return ErrEventInvalid.WithMessage("provider is required for %s", ev.Type)
	}
	if _, err := CodeFor(KindPSPReceivable, ev.Provider); err != nil {
		return ErrEventInvalid.WithMessage("provider %q is not a valid account qualifier", ev.Provider)
	}
	return nil
}

func requireRefund(ev PaymentEvent) error {
	if ev.RefundID == "" {
		return ErrEventInvalid.WithMessage("refund_id is required for %s", ev.Type)
	}
	return nil
}

func requireDispute(ev PaymentEvent) error {
	if ev.DisputeID == "" {
		return ErrEventInvalid.WithMessage("dispute_id is required for %s", ev.Type)
	}
	return nil
}

func entry(acct AccountKey, dir Direction, amt money.Money, desc string) Entry {
	return Entry{Account: acct, Direction: dir, Amount: amt, Description: desc}
}

// J-CAP：Dr psp_receivable[provider] amount；Cr merchant_payable[M] amount − fee；Cr fee_revenue fee。
func templateCapture(ev PaymentEvent) (*Journal, error) {
	if err := requireProvider(ev); err != nil {
		return nil, err
	}
	j, err := baseJournal(ev, TemplateJCAP, RefPayment, ev.PaymentID, "Capture "+ev.PaymentID)
	if err != nil {
		return nil, err
	}
	cur, live := ev.Amount.Currency, ev.Livemode
	fee := money.Zero(cur)
	if ev.HasFee() {
		fee = ev.Fee
	}
	if fee.AmountMinor > ev.Amount.AmountMinor {
		return nil, ErrFeeExceedsAmount.WithMessage("fee %s exceeds captured amount %s", fee, ev.Amount)
	}
	net, err := ev.Amount.Sub(fee)
	if err != nil {
		return nil, ErrEventInvalid.Wrap(err)
	}
	j.Entries = append(j.Entries,
		entry(PSPReceivable(ev.Provider, cur, live), Debit, ev.Amount, "captured gross"),
	)
	if net.IsPositive() {
		j.Entries = append(j.Entries, entry(MerchantPayable(ev.MerchantID, cur, live), Credit, net, "captured net of fee"))
	}
	if fee.IsPositive() {
		j.Entries = append(j.Entries, entry(FeeRevenue(cur, live), Credit, fee, "platform fee"))
	}
	err = j.Validate()
	return j, err
}

// J-REF-PEND：Dr merchant_payable[M]；Cr refund_clearing[M]（先扣商戶餘額、掛清算）。
func templateRefundPending(ev PaymentEvent) (*Journal, error) {
	if err := requireRefund(ev); err != nil {
		return nil, err
	}
	j, err := baseJournal(ev, TemplateJREFPEND, RefRefund, ev.RefundID, "Refund pending "+ev.RefundID)
	if err != nil {
		return nil, err
	}
	cur, live := ev.Amount.Currency, ev.Livemode
	j.Entries = []Entry{
		entry(MerchantPayable(ev.MerchantID, cur, live), Debit, ev.Amount, "refund reserved from merchant balance"),
		entry(RefundClearing(ev.MerchantID, cur, live), Credit, ev.Amount, "refund awaiting PSP confirmation"),
	}
	err = j.Validate()
	return j, err
}

// J-REF-OK：Dr refund_clearing[M]；Cr psp_receivable[provider]。
// J-REF-FEE（fee > 0）：Dr merchant_payable[M]；Cr fee_revenue（退款固定費，docs/02 §7.3）。
func templateRefundSucceeded(ev PaymentEvent) (*Journal, error) {
	if err := requireRefund(ev); err != nil {
		return nil, err
	}
	if err := requireProvider(ev); err != nil {
		return nil, err
	}
	j, err := baseJournal(ev, TemplateJREFOK, RefRefund, ev.RefundID, "Refund succeeded "+ev.RefundID)
	if err != nil {
		return nil, err
	}
	cur, live := ev.Amount.Currency, ev.Livemode
	j.Entries = []Entry{
		entry(RefundClearing(ev.MerchantID, cur, live), Debit, ev.Amount, "refund cleared"),
		entry(PSPReceivable(ev.Provider, cur, live), Credit, ev.Amount, "refund deducted from PSP receivable"),
	}
	if ev.HasFee() {
		j.Entries = append(j.Entries,
			entry(MerchantPayable(ev.MerchantID, cur, live), Debit, ev.Fee, TemplateJREFFEE+": refund fee"),
			entry(FeeRevenue(cur, live), Credit, ev.Fee, TemplateJREFFEE+": refund fee revenue"),
		)
	}
	err = j.Validate()
	return j, err
}

// J-REF-FAIL：Dr refund_clearing[M]；Cr merchant_payable[M]（沖回 J-REF-PEND；reversal_of 由 app 層查出原 journal 後填入）。
func templateRefundFailed(ev PaymentEvent) (*Journal, error) {
	if err := requireRefund(ev); err != nil {
		return nil, err
	}
	j, err := baseJournal(ev, TemplateJREFFail, RefRefund, ev.RefundID, "Refund failed "+ev.RefundID)
	if err != nil {
		return nil, err
	}
	cur, live := ev.Amount.Currency, ev.Livemode
	j.Entries = []Entry{
		entry(RefundClearing(ev.MerchantID, cur, live), Debit, ev.Amount, "refund clearing released"),
		entry(MerchantPayable(ev.MerchantID, cur, live), Credit, ev.Amount, "refund returned to merchant balance"),
	}
	err = j.Validate()
	return j, err
}

// J-CB-OPEN：Dr merchant_payable[M] amount；Cr chargeback_reserve[M] amount；
// 拒付費（fee > 0）：Dr merchant_payable[M] fee；Cr chargeback_fee_revenue fee。
//
// TODO(ledger/proto): DisputeOpened 沒有 stage 欄位（docs/02 §6.2：inquiry 不記帳、chargeback 才記），
// v1 把所有 dispute.opened 視為 chargeback；待 proto 補上 stage 後於此過濾。
func templateDisputeOpened(ev PaymentEvent) (*Journal, error) {
	if err := requireDispute(ev); err != nil {
		return nil, err
	}
	j, err := baseJournal(ev, TemplateJCBOpen, RefDispute, ev.DisputeID, "Dispute opened "+ev.DisputeID)
	if err != nil {
		return nil, err
	}
	cur, live := ev.Amount.Currency, ev.Livemode
	j.Entries = []Entry{
		entry(MerchantPayable(ev.MerchantID, cur, live), Debit, ev.Amount, "disputed amount withheld"),
		entry(ChargebackReserve(ev.MerchantID, cur, live), Credit, ev.Amount, "chargeback reserve"),
	}
	if ev.HasFee() {
		j.Entries = append(j.Entries,
			entry(MerchantPayable(ev.MerchantID, cur, live), Debit, ev.Fee, "chargeback fee charged"),
			entry(ChargebackFeeRevenue(cur, live), Credit, ev.Fee, "chargeback fee revenue"),
		)
	}
	err = j.Validate()
	return j, err
}

// J-CB-LOST：Dr chargeback_reserve[M]；Cr psp_receivable[provider]。
func templateDisputeLost(ev PaymentEvent) (*Journal, error) {
	if err := requireDispute(ev); err != nil {
		return nil, err
	}
	if err := requireProvider(ev); err != nil {
		return nil, err
	}
	j, err := baseJournal(ev, TemplateJCBLost, RefDispute, ev.DisputeID, "Dispute lost "+ev.DisputeID)
	if err != nil {
		return nil, err
	}
	cur, live := ev.Amount.Currency, ev.Livemode
	j.Entries = []Entry{
		entry(ChargebackReserve(ev.MerchantID, cur, live), Debit, ev.Amount, "reserve consumed by chargeback"),
		entry(PSPReceivable(ev.Provider, cur, live), Credit, ev.Amount, "chargeback deducted by PSP"),
	}
	err = j.Validate()
	return j, err
}

// J-CB-WON：Dr chargeback_reserve[M]；Cr merchant_payable[M]。
// J-CB-WON-FEE（政策開啟且 fee > 0）：Dr chargeback_fee_revenue；Cr merchant_payable[M]。
func templateDisputeWon(ev PaymentEvent, pol Policy) (*Journal, error) {
	if err := requireDispute(ev); err != nil {
		return nil, err
	}
	j, err := baseJournal(ev, TemplateJCBWon, RefDispute, ev.DisputeID, "Dispute won "+ev.DisputeID)
	if err != nil {
		return nil, err
	}
	cur, live := ev.Amount.Currency, ev.Livemode
	j.Entries = []Entry{
		entry(ChargebackReserve(ev.MerchantID, cur, live), Debit, ev.Amount, "reserve released"),
		entry(MerchantPayable(ev.MerchantID, cur, live), Credit, ev.Amount, "disputed amount returned to merchant"),
	}
	if pol.RefundChargebackFeeOnWin && ev.HasFee() {
		j.Entries = append(j.Entries,
			entry(ChargebackFeeRevenue(cur, live), Debit, ev.Fee, TemplateJCBWonFee+": chargeback fee refunded"),
			entry(MerchantPayable(ev.MerchantID, cur, live), Credit, ev.Fee, TemplateJCBWonFee+": chargeback fee returned"),
		)
	}
	err = j.Validate()
	return j, err
}

// SettlementTemplate 產生 J-STL：Dr bank_cash[bank] net_paid；Dr psp_fee_expense[provider] psp_fees；Cr psp_receivable[provider] gross。
// 一批結算一筆 journal（系統 journal，MerchantID = uuid.Nil）。
func SettlementTemplate(s SettlementPosted) (*Journal, error) {
	if s.EventID == uuid.Nil || s.SettlementID == "" || s.Provider == "" || s.BankAccount == "" {
		return nil, ErrEventInvalid.WithMessage("settlement requires event_id, settlement_id, provider and bank_account")
	}
	for _, m := range []money.Money{s.Gross, s.PSPFees, s.NetPaid} {
		if err := m.Validate(); err != nil {
			return nil, ErrEventInvalid.WithMessage("settlement amounts: %v", err)
		}
	}
	if s.Gross.Currency != s.PSPFees.Currency || s.Gross.Currency != s.NetPaid.Currency {
		return nil, ErrJournalCurrencyMismatch.WithMessage("settlement amounts must share one currency")
	}
	sum, err := s.NetPaid.Add(s.PSPFees)
	if err != nil {
		return nil, ErrEventInvalid.Wrap(err)
	}
	if !sum.Equal(s.Gross) {
		return nil, ErrJournalUnbalanced.WithMessage("settlement gross %s != net_paid %s + psp_fees %s (post J-STL-DIFF for the difference)", s.Gross, s.NetPaid, s.PSPFees)
	}
	if !s.Gross.IsPositive() {
		return nil, ErrEventInvalid.WithMessage("settlement gross must be > 0")
	}
	cur, live := s.Gross.Currency, s.Livemode
	j := &Journal{
		EventID:       s.EventID,
		MerchantID:    uuid.Nil,
		Livemode:      live,
		SourceType:    SourceReconciliationAdjustment,
		SourceID:      s.SettlementID,
		ReferenceType: RefSettlement,
		ReferenceID:   s.SettlementID,
		Description:   "Settlement " + s.SettlementID + " from " + s.Provider,
		Template:      TemplateJSTL,
		EffectiveAt:   s.OccurredAt,
		PostedAt:      s.OccurredAt,
		Metadata:      map[string]string{MetaProvider: s.Provider},
	}
	if s.NetPaid.IsPositive() {
		j.Entries = append(j.Entries, entry(BankCash(s.BankAccount, cur, live), Debit, s.NetPaid, "settlement net paid"))
	}
	if s.PSPFees.IsPositive() {
		j.Entries = append(j.Entries, entry(PSPFeeExpense(s.Provider, cur, live), Debit, s.PSPFees, "PSP fees"))
	}
	j.Entries = append(j.Entries, entry(PSPReceivable(s.Provider, cur, live), Credit, s.Gross, "settlement gross"))
	err = j.Validate()
	return j, err
}

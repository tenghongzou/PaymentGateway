package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// EventType 為 payment.events 的事件名稱（PaymentEventType 去前綴、小寫、第一個 _ 轉 .）。
type EventType string

// payment.events 事件類型（api/proto/pg/payment/v1/events.proto）。
const (
	EventPaymentCreated           EventType = "payment.created"
	EventPaymentRequiresAction    EventType = "payment.requires_action"
	EventPaymentAuthorized        EventType = "payment.authorized"
	EventPaymentCaptured          EventType = "payment.captured"
	EventPaymentVoided            EventType = "payment.voided"
	EventPaymentFailed            EventType = "payment.failed"
	EventPaymentExpired           EventType = "payment.expired"
	EventRefundCreated            EventType = "refund.created"
	EventRefundSucceeded          EventType = "refund.succeeded"
	EventRefundFailed             EventType = "refund.failed"
	EventDisputeOpened            EventType = "dispute.opened"
	EventDisputeEvidenceSubmitted EventType = "dispute.evidence_submitted"
	EventDisputeWon               EventType = "dispute.won"
	EventDisputeLost              EventType = "dispute.lost"
)

// PaymentEvent 為 domain 視角的付款事件（由 app 層從 protobuf PaymentEvent 轉換而來；
// domain 不 import 產生的 protobuf 型別）。只保留記帳需要的欄位。
type PaymentEvent struct {
	// EventID 為 evt_ ULID 解出的 uuid（journals.event_id / processed_events.event_id）。
	EventID uuid.UUID
	// EventPublicID 為原始 evt_ 字串（寫入 metadata / 描述）。
	EventPublicID string
	Type          EventType
	OccurredAt    time.Time
	// MerchantID 為 mch_ 解出的 uuid。
	MerchantID       uuid.UUID
	MerchantPublicID string
	PaymentID        string
	Livemode         bool
	PaymentVersion   int64
	// Provider 為處理此事件的 PSP（psp_receivable:<provider> 的後綴）。
	Provider string
	// Amount 為事件主金額：captured amount / refund amount / dispute amount。
	Amount money.Money
	// Fee 為事件附帶的手續費：captured 的平台手續費、refund 的退款費、dispute 的拒付費。可為零值（Currency 空）。
	Fee money.Money
	// RefundID / DisputeID 在對應事件上填入（re_ / dp_）。
	RefundID  string
	DisputeID string
}

// HasFee 回傳事件是否帶有 > 0 的手續費。
func (e PaymentEvent) HasFee() bool { return e.Fee.AmountMinor > 0 }

// SettlementPosted 為 reconciliation.events 的 settlement.posted 摘要（J-STL 輸入）。
//
// v1 尚無對應 protobuf 契約（附錄 A 只列出 topic），本型別先提供 domain 範本與測試；
// TODO(ledger/recon): 待 pg.reconciliation.v1 事件定義後，在 adapter/kafka 增加 consumer。
type SettlementPosted struct {
	EventID      uuid.UUID
	SettlementID string
	Provider     string
	// BankAccount 為撥付進來的銀行帳戶代碼（bank_cash:<bank_account>）。
	BankAccount string
	Livemode    bool
	OccurredAt  time.Time
	// Gross 為 PSP 結算的毛額（沖減 psp_receivable）；PSPFees 為 PSP 成本；NetPaid 為實際入帳現金。
	// 必須滿足 Gross = NetPaid + PSPFees（差異請走 J-STL-DIFF）。
	Gross   money.Money
	PSPFees money.Money
	NetPaid money.Money
}

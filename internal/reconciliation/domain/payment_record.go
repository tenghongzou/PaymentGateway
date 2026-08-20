package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// RecordKind 為讀模型紀錄類型（payment_records.kind CHECK）。
type RecordKind string

// 讀模型紀錄類型全集。
const (
	RecordPayment RecordKind = "payment"
	RecordRefund  RecordKind = "refund"
	RecordDispute RecordKind = "dispute"
)

// LineType 回傳此紀錄類型對應的結算列類型。
func (k RecordKind) LineType() LineType {
	switch k {
	case RecordPayment:
		return LinePayment
	case RecordRefund:
		return LineRefund
	case RecordDispute:
		return LineChargeback
	}
	return ""
}

// RecordKindForLine 回傳結算列類型對應的讀模型類型；fee / adjustment 無對應。
func RecordKindForLine(t LineType) (RecordKind, bool) {
	switch t {
	case LinePayment:
		return RecordPayment, true
	case LineRefund:
		return RecordRefund, true
	case LineChargeback:
		return RecordDispute, true
	}
	return "", false
}

// 讀模型狀態（與 payment-service 的狀態字串一致；docs/02 §3 / §5 / §6）。
const (
	StatusAuthorized        = "authorized"
	StatusCaptured          = "captured"
	StatusPartiallyRefunded = "partially_refunded"
	StatusRefunded          = "refunded"
	StatusDisputed          = "disputed"
	StatusChargebackWon     = "chargeback_won"
	StatusChargebackLost    = "chargeback_lost"
	StatusVoided            = "voided"
	StatusFailed            = "failed"
	StatusExpired           = "expired"

	RefundPending   = "pending"
	RefundSucceeded = "succeeded"
	RefundFailed    = "failed"

	DisputeOpen = "open"
	DisputeWon  = "won"
	DisputeLost = "lost"
)

// PaymentRecord 為由 payment.events 投影而來的讀模型（對齊 payment_records 表）。
type PaymentRecord struct {
	ID                uuid.UUID // payment-service 的 payments.id / refunds.id / disputes.id
	Kind              RecordKind
	PublicID          string // pay_ / re_ / dp_
	MerchantID        uuid.UUID
	Provider          string
	ProviderReference string
	Amount            money.Money // payment：累計已請款；refund：退款金額；dispute：爭議金額
	// Fee 為事件中 PSP 同步提供的手續費；nil 表示未知（不做 fee 比對）。
	// TODO(migration)：payment_records 目前沒有 fee 欄位，postgres adapter 無法持久化，
	// 需要 migrations/reconciliation 新增 `fee bigint` 後才能在正式路徑產生 fee_mismatch。
	Fee        *money.Money
	Status     string
	OccurredAt time.Time
	SourceSeq  int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// IsSettleable 回傳此紀錄是否「預期會出現在 PSP 結算檔」：
// payment 已請款（含之後退款 / 爭議的狀態）、refund 成功、dispute 敗訴。
func (r PaymentRecord) IsSettleable() bool {
	switch r.Kind {
	case RecordPayment:
		switch r.Status {
		case StatusCaptured, StatusPartiallyRefunded, StatusRefunded, StatusDisputed, StatusChargebackWon, StatusChargebackLost:
			return true
		}
	case RecordRefund:
		return r.Status == RefundSucceeded
	case RecordDispute:
		return r.Status == DisputeLost
	}
	return false
}

// SettleableStatuses 回傳各 kind 視為「應結算」的狀態清單（供 repository 組 SQL）。
func SettleableStatuses() map[RecordKind][]string {
	return map[RecordKind][]string{
		RecordPayment: {StatusCaptured, StatusPartiallyRefunded, StatusRefunded, StatusDisputed, StatusChargebackWon, StatusChargebackLost},
		RecordRefund:  {RefundSucceeded},
		RecordDispute: {DisputeLost},
	}
}

// Key 回傳此紀錄的比對鍵。
func (r PaymentRecord) Key() MatchKey {
	return MatchKey{ProviderReference: r.ProviderReference, Type: r.Kind.LineType()}
}

// Package domain 為 payment-service 的領域層：Payment / Attempt / Refund 聚合、狀態機、領域事件與錯誤。
// 本套件不得 import app / adapter 或任何基礎設施（docs/01 §7 import 規則）。
package domain

// Status 為 Payment 狀態（與 migrations/payment 的 CHECK 一致）。
type Status string

// Payment 狀態列舉（docs/02 §3.1）。
const (
	StatusCreated           Status = "created"
	StatusRequiresAction    Status = "requires_action"
	StatusAuthorized        Status = "authorized"
	StatusCaptured          Status = "captured"
	StatusPartiallyRefunded Status = "partially_refunded"
	StatusRefunded          Status = "refunded"
	StatusVoided            Status = "voided"
	StatusFailed            Status = "failed"
	StatusExpired           Status = "expired"
	StatusDisputed          Status = "disputed"
	StatusChargebackWon     Status = "chargeback_won"
	StatusChargebackLost    Status = "chargeback_lost"
)

// AllStatuses 列出所有狀態（測試用）。
var AllStatuses = []Status{
	StatusCreated, StatusRequiresAction, StatusAuthorized, StatusCaptured, StatusPartiallyRefunded,
	StatusRefunded, StatusVoided, StatusFailed, StatusExpired, StatusDisputed, StatusChargebackWon, StatusChargebackLost,
}

// IsTerminal 回傳是否為終態（docs/02 §3.1）。
func (s Status) IsTerminal() bool {
	switch s {
	case StatusVoided, StatusFailed, StatusExpired, StatusChargebackLost:
		return true
	case StatusCreated, StatusRequiresAction, StatusAuthorized, StatusCaptured, StatusPartiallyRefunded,
		StatusRefunded, StatusDisputed, StatusChargebackWon:
		return false
	default:
		return false
	}
}

// IsValid 檢查是否為已知狀態。
func (s Status) IsValid() bool {
	for _, v := range AllStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// transitions 為合法轉移表（docs/02 §3.2 T1–T21）。
var transitions = map[Status][]Status{
	StatusCreated:           {StatusRequiresAction, StatusAuthorized, StatusCaptured, StatusFailed, StatusExpired},
	StatusRequiresAction:    {StatusAuthorized, StatusCaptured, StatusFailed, StatusExpired, StatusVoided},
	StatusAuthorized:        {StatusCaptured, StatusVoided},
	StatusCaptured:          {StatusPartiallyRefunded, StatusRefunded, StatusDisputed},
	StatusPartiallyRefunded: {StatusPartiallyRefunded, StatusRefunded, StatusDisputed},
	StatusRefunded:          {StatusDisputed},
	StatusDisputed:          {StatusChargebackWon, StatusChargebackLost},
	StatusChargebackWon:     {StatusDisputed, StatusPartiallyRefunded, StatusRefunded},
	StatusVoided:            {},
	StatusFailed:            {},
	StatusExpired:           {},
	StatusChargebackLost:    {},
}

// CanTransition 判斷 from → to 是否為合法轉移。
func CanTransition(from, to Status) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// CaptureMethod 為請款方式。
type CaptureMethod string

// 請款方式列舉。
const (
	CaptureAutomatic CaptureMethod = "automatic"
	CaptureManual    CaptureMethod = "manual"
)

// AttemptStatus 為 PaymentAttempt 狀態（docs/02 §4.1，與 SQL 一致）。
type AttemptStatus string

// Attempt 狀態列舉。
const (
	AttemptPending        AttemptStatus = "pending"
	AttemptRequiresAction AttemptStatus = "requires_action"
	AttemptApproved       AttemptStatus = "approved"
	AttemptDeclined       AttemptStatus = "declined"
	AttemptUnavailable    AttemptStatus = "unavailable"
	AttemptUnknown        AttemptStatus = "unknown"
)

// IsTerminal 回傳 Attempt 是否為終態。
func (s AttemptStatus) IsTerminal() bool {
	return s == AttemptApproved || s == AttemptDeclined || s == AttemptUnavailable
}

// IsOpen 回傳 Attempt 是否仍進行中（pending / requires_action / unknown）。
func (s AttemptStatus) IsOpen() bool {
	return s == AttemptPending || s == AttemptRequiresAction || s == AttemptUnknown
}

// RefundStatus 為 Refund 狀態。
type RefundStatus string

// Refund 狀態列舉。
const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

// VoidReason 為 payments.void_reason 的合法值。
type VoidReason string

// Void 原因列舉（與 SQL CHECK 一致）。
const (
	VoidReasonMerchantRequest      VoidReason = "merchant_request"
	VoidReasonAuthorizationExpired VoidReason = "authorization_expired"
	VoidReasonCaptureFailedCleanup VoidReason = "capture_failed_cleanup"
	VoidReasonRiskDecline          VoidReason = "risk_decline"
)

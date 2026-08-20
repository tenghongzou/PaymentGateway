package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// Attempt 為一次對 Provider 的 authorize 呼叫（對齊 payment_attempts 表）。
type Attempt struct {
	ID                string // uuid
	PaymentID         string // uuid
	MerchantID        string
	AttemptNo         int
	Operation         string // authorize
	Provider          string
	ProviderReference string
	Status            AttemptStatus
	ErrorCategory     ProviderErrorCategory
	ErrorCode         string
	ErrorMessage      string
	RouteReason       string
	// NextAction 在 requires_action 時保存（落庫於 response_snapshot.next_action）。
	NextAction  *NextAction
	LatencyMs   *int
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// NewAttempt 建立 pending 的 Attempt。
func NewAttempt(p *Payment, provider, routeReason string, no int, now time.Time) *Attempt {
	return &Attempt{
		ID:          ids.NewUUID().String(),
		PaymentID:   p.ID,
		MerchantID:  p.MerchantID,
		AttemptNo:   no,
		Operation:   "authorize",
		Provider:    provider,
		Status:      AttemptPending,
		RouteReason: routeReason,
		CreatedAt:   now,
	}
}

// PublicID 回傳 att_ 公開 ID（由 uuid 推導，SQL 無 public_id 欄位）。
func (a *Attempt) PublicID() string {
	u, err := uuid.Parse(a.ID)
	if err != nil {
		return "att_" + a.ID
	}
	return ids.Format(ids.PrefixAttempt, u)
}

func (a *Attempt) complete(now time.Time) {
	a.CompletedAt = &now
	ms := int(now.Sub(a.CreatedAt).Milliseconds())
	if ms < 0 {
		ms = 0
	}
	a.LatencyMs = &ms
}

// MarkApproved 標記核准。
func (a *Attempt) MarkApproved(providerRef string, now time.Time) {
	a.Status = AttemptApproved
	a.ProviderReference = providerRef
	a.ErrorCategory, a.ErrorCode, a.ErrorMessage = CategoryNone, "", ""
	a.complete(now)
}

// MarkRequiresAction 標記需要客戶動作（仍為進行中）。
func (a *Attempt) MarkRequiresAction(providerRef string, next *NextAction, now time.Time) {
	a.Status = AttemptRequiresAction
	a.ProviderReference = providerRef
	a.NextAction = next
	a.ErrorCategory = CategoryAuthenticationRequired
	a.complete(now)
}

// MarkFailed 依 ProviderErrorCategory 決定 declined / unavailable / unknown（docs/02 §11.0）。
func (a *Attempt) MarkFailed(cat ProviderErrorCategory, code, message, providerRef string, now time.Time) {
	a.Status = cat.AttemptStatus()
	if a.Status == AttemptRequiresAction {
		// authentication_required 不是失敗；呼叫端應用 MarkRequiresAction。
		a.Status = AttemptDeclined
	}
	a.ErrorCategory = cat
	a.ErrorCode = code
	a.ErrorMessage = message
	if providerRef != "" {
		a.ProviderReference = providerRef
	}
	a.complete(now)
}

// Resolve 把 unknown 收斂為最終狀態（GetPaymentStatus 結果）。
func (a *Attempt) Resolve(status AttemptStatus, providerRef string, now time.Time) {
	a.Status = status
	if providerRef != "" {
		a.ProviderReference = providerRef
	}
	a.complete(now)
}

// CanFailover 判斷此 Attempt 的結果是否允許切換 Provider（docs/02 §9.5）。
func (a *Attempt) CanFailover() bool {
	switch a.Status {
	case AttemptUnavailable:
		return true
	case AttemptDeclined:
		return CanFailoverDecline(a.ErrorCategory, a.ErrorCode)
	case AttemptPending, AttemptRequiresAction, AttemptApproved, AttemptUnknown:
		return false
	default:
		return false
	}
}

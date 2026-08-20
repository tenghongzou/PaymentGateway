package domain

import (
	"time"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// 合法的退款原因（refunds.reason CHECK）。
var refundReasons = map[string]bool{"requested_by_customer": true, "duplicate": true, "fraudulent": true, "other": true}

// IsValidRefundReason 檢查退款原因。
func IsValidRefundReason(r string) bool { return r == "" || refundReasons[r] }

// Refund 為退款聚合根（對齊 refunds 表）。
type Refund struct {
	ID                string // uuid
	PublicID          string // re_...
	PaymentID         string // uuid
	PaymentPublicID   string // pay_...（讀取時帶出，方便組回應）
	MerchantID        string
	IdempotencyKey    string
	Amount            money.Money
	Status            RefundStatus
	Reason            string
	Provider          string
	ProviderReference string
	FailureCode       string
	FailureMessage    string
	Metadata          map[string]string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SucceededAt       *time.Time
	Version           int
	LiveMode          bool
}

// NewRefund 建立 pending 的 Refund（約束檢查由 Payment.ReserveRefund 負責）。
func NewRefund(p *Payment, idempotencyKey string, amount money.Money, reason string, metadata map[string]string, now time.Time) (*Refund, error) {
	if !IsValidRefundReason(reason) {
		return nil, ErrInvalidTransition.WithMessage("reason must be one of requested_by_customer|duplicate|fraudulent|other").WithParam("reason")
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	u := ids.NewUUID()
	return &Refund{
		ID:              u.String(),
		PublicID:        ids.Format(ids.PrefixRefund, u),
		PaymentID:       p.ID,
		PaymentPublicID: p.PublicID,
		MerchantID:      p.MerchantID,
		IdempotencyKey:  idempotencyKey,
		Amount:          amount,
		Status:          RefundPending,
		Reason:          reason,
		Provider:        p.SelectedProvider,
		Metadata:        metadata,
		CreatedAt:       now,
		UpdatedAt:       now,
		LiveMode:        p.LiveMode,
	}, nil
}

// Succeed 標記退款成功。
func (r *Refund) Succeed(providerRef string, now time.Time) error {
	if r.Status != RefundPending {
		return ErrInvalidTransition.WithMessage("refund %s is already %s", r.PublicID, r.Status)
	}
	r.Status = RefundSucceeded
	r.ProviderReference = providerRef
	r.SucceededAt = &now
	r.UpdatedAt = now
	r.Version++
	return nil
}

// Fail 標記退款失敗。
func (r *Refund) Fail(code, message string, now time.Time) error {
	if r.Status != RefundPending {
		return ErrInvalidTransition.WithMessage("refund %s is already %s", r.PublicID, r.Status)
	}
	r.Status = RefundFailed
	r.FailureCode = code
	r.FailureMessage = message
	r.UpdatedAt = now
	r.Version++
	return nil
}

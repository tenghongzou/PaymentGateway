package domain

import (
	"time"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// 事件型別（payment_events.event_type 與 Kafka event_type）。
const (
	EventPaymentCreated        = "payment.created"
	EventPaymentRequiresAction = "payment.requires_action"
	EventPaymentAuthorized     = "payment.authorized"
	EventPaymentCaptured       = "payment.captured"
	EventPaymentVoided         = "payment.voided"
	EventPaymentFailed         = "payment.failed"
	EventPaymentExpired        = "payment.expired"
	EventRefundCreated         = "refund.created"
	EventRefundSucceeded       = "refund.succeeded"
	EventRefundFailed          = "refund.failed"
)

// 預設時限。
const (
	// DefaultActionWindow 為 requires_action 的完成期限（docs/02 T1）。
	DefaultActionWindow = 30 * time.Minute
	// DefaultCreatedTTL 為 created 卡住視為 expired 的時間（docs/02 T5）。
	DefaultCreatedTTL = time.Hour
	// DefaultAuthValidity 為未取得 PSP 授權期限時的預設值（7 天）。
	DefaultAuthValidity = 7 * 24 * time.Hour
)

// Event 為一次狀態轉移產生的領域事件；Seq = 轉移後的 Payment.Version（payment_events.seq）。
type Event struct {
	Type       string
	FromStatus *Status
	ToStatus   Status
	Seq        int
	OccurredAt time.Time
	// Payload 為事件附帶資料（寫入 payment_events.payload jsonb；adapter 另外序列化成 protobuf 送 outbox）。
	Payload map[string]any
}

// Customer 為客戶資訊值物件（PII，落庫 jsonb）。
type Customer struct {
	ID                string `json:"id,omitempty"`
	Email             string `json:"email,omitempty"`
	Name              string `json:"name,omitempty"`
	Phone             string `json:"phone,omitempty"`
	IPAddress         string `json:"ip_address,omitempty"`
	UserAgent         string `json:"user_agent,omitempty"`
	BillingCountry    string `json:"billing_country,omitempty"`
	BillingPostalCode string `json:"billing_postal_code,omitempty"`
}

// PaymentMethodDetails 為付款工具的非敏感資訊（jsonb 白名單：絕不含 PAN / CVC）。
type PaymentMethodDetails struct {
	Brand         string `json:"brand,omitempty"`
	Last4         string `json:"last4,omitempty"`
	ExpMonth      int    `json:"exp_month,omitempty"`
	ExpYear       int    `json:"exp_year,omitempty"`
	Issuer        string `json:"issuer,omitempty"`
	IssuerCountry string `json:"issuer_country,omitempty"`
	Funding       string `json:"funding,omitempty"`
	ThreeDSResult string `json:"three_ds_result,omitempty"`
	WalletType    string `json:"wallet_type,omitempty"`
	BankCountry   string `json:"bank_country,omitempty"`
	BankCode      string `json:"bank_code,omitempty"`
	// TokenProvider 為發 token 的 PSP（路由鎖定用，非敏感）。
	TokenProvider string `json:"token_provider,omitempty"`
}

// NextAction 為 requires_action 時要回給商戶的動作（存在 current attempt 的 response_snapshot）。
type NextAction struct {
	Type       string            `json:"type"` // redirect | three_ds_challenge | display
	URL        string            `json:"url,omitempty"`
	Method     string            `json:"method,omitempty"`
	FormFields map[string]string `json:"form_fields,omitempty"`
	ACSURL     string            `json:"acs_url,omitempty"`
	CReq       string            `json:"creq,omitempty"`
	TxnID      string            `json:"transaction_id,omitempty"`
	Version    string            `json:"version,omitempty"`
	Display    map[string]string `json:"display,omitempty"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

// Payment 為付款聚合根（欄位對齊 migrations/payment/0001_schema.up.sql 的 payments 表）。
type Payment struct {
	ID                     string // uuid
	PublicID               string // pay_...
	MerchantID             string // uuid
	IdempotencyKey         string
	IdempotencyRequestHash string
	Amount                 money.Money
	CaptureMethod          CaptureMethod
	Status                 Status
	AmountAuthorized       money.Money
	AmountCaptured         money.Money
	AmountRefunded         money.Money
	AmountRefundPending    money.Money
	PaymentMethodType      string // card / wallet / bank_transfer
	PaymentMethodDetails   PaymentMethodDetails
	Customer               Customer
	Description            string
	StatementDescriptor    string
	ReturnURL              string
	SelectedProvider       string
	ProviderReference      string
	Failure                *Failure
	ExpiresAt              *time.Time
	AuthExpiresAt          *time.Time
	AuthorizedAt           *time.Time
	CapturedAt             *time.Time
	VoidReason             *VoidReason
	VoidedAt               *time.Time
	Metadata               map[string]string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	Version                int
	// LiveMode 目前不落庫（SQL 尚無 livemode 欄位），由 gateway 依 API key mode 回填。
	LiveMode bool

	Attempts []*Attempt
}

// NewPaymentParams 為建立 Payment 的輸入。
type NewPaymentParams struct {
	MerchantID          string
	IdempotencyKey      string
	RequestHash         string
	Amount              money.Money
	CaptureMethod       CaptureMethod
	PaymentMethodType   string
	PaymentMethod       PaymentMethodDetails
	Customer            Customer
	Description         string
	StatementDescriptor string
	ReturnURL           string
	Metadata            map[string]string
	LiveMode            bool
}

// NewPayment 建立 created 狀態的 Payment 並回傳 payment.created 事件（version = 1）。
func NewPayment(p NewPaymentParams, now time.Time) (*Payment, Event, error) {
	if err := p.Amount.Validate(); err != nil {
		return nil, Event{}, ErrInvalidCurrency.Wrap(err)
	}
	if !p.Amount.IsPositive() {
		return nil, Event{}, ErrAmountTooSmall
	}
	if p.CaptureMethod == "" {
		p.CaptureMethod = CaptureAutomatic
	}
	if p.CaptureMethod != CaptureAutomatic && p.CaptureMethod != CaptureManual {
		return nil, Event{}, ErrInvalidTransition.WithMessage("capture_method must be automatic or manual").WithParam("capture_method")
	}
	if p.PaymentMethodType == "" {
		return nil, Event{}, ErrPaymentMethodMissing
	}
	if len(p.Metadata) > 50 {
		return nil, Event{}, ErrMetadataTooLarge
	}
	for k, v := range p.Metadata {
		if len(k) > 40 || len(v) > 500 {
			return nil, Event{}, ErrMetadataTooLarge
		}
	}
	u := ids.NewUUID()
	expires := now.Add(DefaultCreatedTTL)
	pay := &Payment{
		ID:                     u.String(),
		PublicID:               ids.Format(ids.PrefixPayment, u),
		MerchantID:             p.MerchantID,
		IdempotencyKey:         p.IdempotencyKey,
		IdempotencyRequestHash: p.RequestHash,
		Amount:                 p.Amount,
		CaptureMethod:          p.CaptureMethod,
		Status:                 StatusCreated,
		AmountAuthorized:       money.Zero(p.Amount.Currency),
		AmountCaptured:         money.Zero(p.Amount.Currency),
		AmountRefunded:         money.Zero(p.Amount.Currency),
		AmountRefundPending:    money.Zero(p.Amount.Currency),
		PaymentMethodType:      p.PaymentMethodType,
		PaymentMethodDetails:   p.PaymentMethod,
		Customer:               p.Customer,
		Description:            p.Description,
		StatementDescriptor:    p.StatementDescriptor,
		ReturnURL:              p.ReturnURL,
		ExpiresAt:              &expires,
		Metadata:               p.Metadata,
		CreatedAt:              now,
		UpdatedAt:              now,
		Version:                0,
		LiveMode:               p.LiveMode,
	}
	if pay.Metadata == nil {
		pay.Metadata = map[string]string{}
	}
	ev := pay.emit(EventPaymentCreated, nil, StatusCreated, now, map[string]any{
		"amount": pay.Amount.AmountMinor, "currency": pay.Amount.Currency,
		"capture_method": string(pay.CaptureMethod), "payment_method_type": pay.PaymentMethodType,
	})
	return pay, ev, nil
}

// emit 套用狀態並產生事件（version +1，seq = 新 version）。
func (p *Payment) emit(typ string, from *Status, to Status, now time.Time, payload map[string]any) Event {
	p.Status = to
	p.Version++
	p.UpdatedAt = now
	if payload == nil {
		payload = map[string]any{}
	}
	return Event{Type: typ, FromStatus: from, ToStatus: to, Seq: p.Version, OccurredAt: now, Payload: payload}
}

func (p *Payment) transition(typ string, to Status, now time.Time, payload map[string]any) (Event, error) {
	from := p.Status
	if !CanTransition(from, to) {
		return Event{}, TransitionError(from, to)
	}
	return p.emit(typ, &from, to, now, payload), nil
}

// CurrentAttempt 回傳最後一個 Attempt（沒有時為 nil）。
func (p *Payment) CurrentAttempt() *Attempt {
	if len(p.Attempts) == 0 {
		return nil
	}
	return p.Attempts[len(p.Attempts)-1]
}

// OpenAttempt 回傳進行中的 Attempt（pending / requires_action / unknown），沒有時為 nil。
func (p *Payment) OpenAttempt() *Attempt {
	for _, a := range p.Attempts {
		if a.Status.IsOpen() {
			return a
		}
	}
	return nil
}

// WinningAttempt 回傳 approved 的 Attempt（沒有時為 nil）。
func (p *Payment) WinningAttempt() *Attempt {
	for _, a := range p.Attempts {
		if a.Status == AttemptApproved {
			return a
		}
	}
	return nil
}

// StartAttempt 建立新的 authorize Attempt（seq = len+1）；已有進行中的 Attempt 時回 ErrOperationInProgress。
func (p *Payment) StartAttempt(provider, routeReason string, now time.Time) (*Attempt, error) {
	if p.Status != StatusCreated {
		return nil, TransitionError(p.Status, StatusCreated).WithMessage("attempts can only be started while the payment is created (current: %s)", p.Status)
	}
	if p.OpenAttempt() != nil {
		return nil, ErrOperationInProgress
	}
	a := NewAttempt(p, provider, routeReason, len(p.Attempts)+1, now)
	p.Attempts = append(p.Attempts, a)
	p.SelectedProvider = provider
	return a, nil
}

// RequireAction 套用 T1：created → requires_action；expiresAt 為零值時用 now + 30m。
func (p *Payment) RequireAction(providerRef string, expiresAt time.Time, now time.Time) (Event, error) {
	if expiresAt.IsZero() {
		expiresAt = now.Add(DefaultActionWindow)
	}
	ev, err := p.transition(EventPaymentRequiresAction, StatusRequiresAction, now, map[string]any{
		"provider": p.SelectedProvider, "expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return Event{}, err
	}
	p.ProviderReference = providerRef
	p.ExpiresAt = &expiresAt
	return ev, nil
}

// AuthorizeParams 為授權成功的結果。
type AuthorizeParams struct {
	Provider          string
	ProviderReference string
	AuthExpiresAt     time.Time
	Details           *PaymentMethodDetails
	FeeMinor          int64
}

// Authorize 套用 T2 / T6：created | requires_action → authorized；authorized_amount = amount。
func (p *Payment) Authorize(ap AuthorizeParams, now time.Time) (Event, error) {
	if p.Status == StatusRequiresAction && p.ExpiresAt != nil && now.After(*p.ExpiresAt) {
		return Event{}, ErrPaymentExpired
	}
	if ap.AuthExpiresAt.IsZero() {
		ap.AuthExpiresAt = now.Add(DefaultAuthValidity)
	}
	ev, err := p.transition(EventPaymentAuthorized, StatusAuthorized, now, map[string]any{
		"amount": p.Amount.AmountMinor, "currency": p.Amount.Currency, "provider": ap.Provider,
		"provider_reference": ap.ProviderReference, "fee": ap.FeeMinor,
		"authorization_expires_at": ap.AuthExpiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return Event{}, err
	}
	p.AmountAuthorized = p.Amount
	if ap.Provider != "" {
		p.SelectedProvider = ap.Provider
	}
	p.ProviderReference = ap.ProviderReference
	p.AuthorizedAt = &now
	p.AuthExpiresAt = &ap.AuthExpiresAt
	p.ExpiresAt = nil
	p.Failure = nil
	if ap.Details != nil {
		p.PaymentMethodDetails = mergeDetails(p.PaymentMethodDetails, *ap.Details)
	}
	return ev, nil
}

// Capture 套用 T11（或 automatic 的 T3 第二段）：authorized → captured；0 < amount ≤ authorized。
func (p *Payment) Capture(amount money.Money, feeMinor int64, now time.Time) (Event, error) {
	if p.Status != StatusAuthorized {
		return Event{}, TransitionError(p.Status, StatusCaptured)
	}
	if amount.Currency != p.Amount.Currency {
		return Event{}, ErrCurrencyMismatch
	}
	if !amount.IsPositive() {
		return Event{}, ErrAmountTooSmall
	}
	if amount.GreaterThan(p.AmountAuthorized) {
		return Event{}, ErrAmountExceedsAuthorized
	}
	if p.AuthExpiresAt != nil && now.After(*p.AuthExpiresAt) {
		return Event{}, ErrPaymentExpired
	}
	ev, err := p.transition(EventPaymentCaptured, StatusCaptured, now, map[string]any{
		"amount": amount.AmountMinor, "currency": amount.Currency, "total_captured": amount.AmountMinor,
		"provider": p.SelectedProvider, "provider_reference": p.ProviderReference, "fee": feeMinor,
		"is_final": true,
	})
	if err != nil {
		return Event{}, err
	}
	p.AmountCaptured = amount
	p.CapturedAt = &now
	return ev, nil
}

// RemainingAuthorized 回傳尚未請款的授權額度。
func (p *Payment) RemainingAuthorized() money.Money {
	m, err := p.AmountAuthorized.Sub(p.AmountCaptured)
	if err != nil {
		return money.Zero(p.Amount.Currency)
	}
	return m
}

// Void 套用 T10 / T12 / T13：requires_action | authorized → voided。
func (p *Payment) Void(reason VoidReason, detail string, now time.Time) (Event, error) {
	switch p.Status {
	case StatusCaptured, StatusPartiallyRefunded, StatusRefunded, StatusDisputed, StatusChargebackWon:
		return Event{}, ErrVoidNotAllowed
	case StatusCreated, StatusRequiresAction, StatusAuthorized, StatusVoided, StatusFailed, StatusExpired, StatusChargebackLost:
	}
	if reason == "" {
		reason = VoidReasonMerchantRequest
	}
	ev, err := p.transition(EventPaymentVoided, StatusVoided, now, map[string]any{
		"amount": p.AmountAuthorized.AmountMinor, "currency": p.Amount.Currency,
		"provider": p.SelectedProvider, "provider_reference": p.ProviderReference,
		"reason": string(reason), "detail": detail,
	})
	if err != nil {
		return Event{}, err
	}
	p.VoidReason = &reason
	p.VoidedAt = &now
	p.ExpiresAt = nil
	return ev, nil
}

// Fail 套用 T4 / T8：created | requires_action → failed；寫入 failure。
func (p *Payment) Fail(f Failure, now time.Time) (Event, error) {
	if f.Provider == "" {
		f.Provider = p.SelectedProvider
	}
	f.Retryable = IsRetryableDecline(f.Category, f.Code)
	ev, err := p.transition(EventPaymentFailed, StatusFailed, now, map[string]any{
		"amount": p.Amount.AmountMinor, "currency": p.Amount.Currency, "provider": f.Provider,
		"error_category": string(f.Category), "error_code": f.PublicCode(), "error_message": f.Message,
		"attempt_count": len(p.Attempts), "retryable": f.Retryable,
	})
	if err != nil {
		return Event{}, err
	}
	p.Failure = &f
	p.ExpiresAt = nil
	return ev, nil
}

// Expire 套用 T5 / T9：created | requires_action → expired。
func (p *Payment) Expire(now time.Time) (Event, error) {
	prev := p.Status
	ev, err := p.transition(EventPaymentExpired, StatusExpired, now, map[string]any{
		"amount": p.Amount.AmountMinor, "currency": p.Amount.Currency,
		"provider": p.SelectedProvider, "previous_status": string(prev),
	})
	if err != nil {
		return Event{}, err
	}
	p.ExpiresAt = nil
	return ev, nil
}

// AvailableToRefund 回傳可退金額 = captured − refunded − refund_pending。
func (p *Payment) AvailableToRefund() money.Money {
	m, err := p.AmountCaptured.Sub(p.AmountRefunded)
	if err != nil {
		return money.Zero(p.Amount.Currency)
	}
	m, err = m.Sub(p.AmountRefundPending)
	if err != nil {
		return money.Zero(p.Amount.Currency)
	}
	return m
}

// CanRefund 檢查是否允許建立退款（docs/02 §5.2）。
func (p *Payment) CanRefund() error {
	switch p.Status {
	case StatusCaptured, StatusPartiallyRefunded, StatusChargebackWon:
		return nil
	case StatusDisputed:
		return ErrPaymentDisputed
	case StatusCreated, StatusRequiresAction, StatusAuthorized, StatusRefunded, StatusVoided, StatusFailed, StatusExpired, StatusChargebackLost:
		return ErrPaymentNotRefundable.WithMessage("payment in status %q cannot be refunded", p.Status)
	default:
		return ErrPaymentNotRefundable
	}
}

// ReserveRefund 為兩階段退款的第一步：檢查約束並預留額度（不改變 Payment 狀態，version +1）。
func (p *Payment) ReserveRefund(r *Refund, now time.Time) (Event, error) {
	if err := p.CanRefund(); err != nil {
		return Event{}, err
	}
	if r.Amount.Currency != p.Amount.Currency {
		return Event{}, ErrCurrencyMismatch
	}
	if !r.Amount.IsPositive() {
		return Event{}, ErrAmountTooSmall
	}
	if r.Amount.GreaterThan(p.AvailableToRefund()) {
		return Event{}, ErrRefundExceedsAvailable.WithMessage("refund amount %s exceeds available %s", r.Amount, p.AvailableToRefund())
	}
	pending, err := p.AmountRefundPending.Add(r.Amount)
	if err != nil {
		return Event{}, ErrRefundExceedsAvailable.Wrap(err)
	}
	p.AmountRefundPending = pending
	from := p.Status
	ev := p.emit(EventRefundCreated, &from, p.Status, now, map[string]any{
		"refund_id": r.PublicID, "amount": r.Amount.AmountMinor, "currency": r.Amount.Currency,
		"provider": p.SelectedProvider, "reason": r.Reason,
	})
	return ev, nil
}

// MarkRefunded 套用 T14–T17 / T21：退款成功 → partially_refunded | refunded。
func (p *Payment) MarkRefunded(r *Refund, now time.Time) (Event, error) {
	pending, err := p.AmountRefundPending.Sub(r.Amount)
	if err != nil {
		return Event{}, ErrRefundExceedsAvailable.Wrap(err)
	}
	refunded, err := p.AmountRefunded.Add(r.Amount)
	if err != nil {
		return Event{}, ErrRefundExceedsAvailable.Wrap(err)
	}
	if refunded.GreaterThan(p.AmountCaptured) {
		return Event{}, ErrRefundExceedsAvailable
	}
	to := StatusPartiallyRefunded
	if refunded.Equal(p.AmountCaptured) {
		to = StatusRefunded
	}
	ev, err := p.transition(EventRefundSucceeded, to, now, map[string]any{
		"refund_id": r.PublicID, "amount": r.Amount.AmountMinor, "currency": r.Amount.Currency,
		"provider": r.Provider, "provider_reference": r.ProviderReference, "total_refunded": refunded.AmountMinor,
	})
	if err != nil {
		return Event{}, err
	}
	p.AmountRefundPending = pending
	p.AmountRefunded = refunded
	return ev, nil
}

// ReleaseRefund 為退款失敗：釋放預留額度（狀態不變，version +1）。
func (p *Payment) ReleaseRefund(r *Refund, now time.Time) (Event, error) {
	pending, err := p.AmountRefundPending.Sub(r.Amount)
	if err != nil {
		return Event{}, ErrRefundExceedsAvailable.Wrap(err)
	}
	p.AmountRefundPending = pending
	from := p.Status
	ev := p.emit(EventRefundFailed, &from, p.Status, now, map[string]any{
		"refund_id": r.PublicID, "amount": r.Amount.AmountMinor, "currency": r.Amount.Currency,
		"provider": r.Provider, "error_code": r.FailureCode, "error_message": r.FailureMessage,
	})
	return ev, nil
}

// mergeDetails 以 PSP 回傳的非敏感資訊補齊既有欄位（不覆寫已有值的 token_provider）。
func mergeDetails(base, in PaymentMethodDetails) PaymentMethodDetails {
	pick := func(a, b string) string {
		if b != "" {
			return b
		}
		return a
	}
	base.Brand = pick(base.Brand, in.Brand)
	base.Last4 = pick(base.Last4, in.Last4)
	base.Issuer = pick(base.Issuer, in.Issuer)
	base.IssuerCountry = pick(base.IssuerCountry, in.IssuerCountry)
	base.Funding = pick(base.Funding, in.Funding)
	base.ThreeDSResult = pick(base.ThreeDSResult, in.ThreeDSResult)
	base.WalletType = pick(base.WalletType, in.WalletType)
	if in.ExpMonth != 0 {
		base.ExpMonth = in.ExpMonth
	}
	if in.ExpYear != 0 {
		base.ExpYear = in.ExpYear
	}
	return base
}

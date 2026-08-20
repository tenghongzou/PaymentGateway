package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

var now = time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

func newTestPayment(t *testing.T, cm CaptureMethod) *Payment {
	t.Helper()
	p, ev, err := NewPayment(NewPaymentParams{
		MerchantID: "m-1", IdempotencyKey: "k-1", Amount: money.MustNew(1000, "TWD"), CaptureMethod: cm,
		PaymentMethodType: "card", PaymentMethod: PaymentMethodDetails{TokenProvider: "mock"}, Metadata: map[string]string{"order": "1"},
	}, now)
	require.NoError(t, err)
	assert.Equal(t, EventPaymentCreated, ev.Type)
	assert.Equal(t, 1, ev.Seq)
	assert.Nil(t, ev.FromStatus)
	assert.Equal(t, StatusCreated, p.Status)
	assert.Equal(t, 1, p.Version)
	assert.True(t, ids.HasPrefix(p.PublicID, ids.PrefixPayment))
	return p
}

func TestNewPaymentValidation(t *testing.T) {
	base := NewPaymentParams{MerchantID: "m", IdempotencyKey: "k", Amount: money.MustNew(100, "TWD"), PaymentMethodType: "card"}
	tests := []struct {
		name    string
		mutate  func(*NewPaymentParams)
		wantErr *apperr.Error
	}{
		{"ok", func(*NewPaymentParams) {}, nil},
		{"zero amount", func(p *NewPaymentParams) { p.Amount = money.Zero("TWD") }, ErrAmountTooSmall},
		{"bad currency", func(p *NewPaymentParams) { p.Amount = money.Money{AmountMinor: 1, Currency: "XXX"} }, ErrInvalidCurrency},
		{"bad capture method", func(p *NewPaymentParams) { p.CaptureMethod = "later" }, ErrInvalidTransition},
		{"no method", func(p *NewPaymentParams) { p.PaymentMethodType = "" }, ErrPaymentMethodMissing},
		{"metadata too many", func(p *NewPaymentParams) {
			p.Metadata = map[string]string{}
			for i := range 51 {
				p.Metadata[string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
			}
		}, ErrMetadataTooLarge},
		{"metadata value too long", func(p *NewPaymentParams) { p.Metadata = map[string]string{"k": string(make([]byte, 501))} }, ErrMetadataTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := base
			tt.mutate(&params)
			p, _, err := NewPayment(params, now)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, CaptureAutomatic, p.CaptureMethod)
			assert.NotNil(t, p.Metadata)
			assert.Equal(t, now.Add(DefaultCreatedTTL), *p.ExpiresAt)
		})
	}
}

func TestAutomaticHappyPath(t *testing.T) {
	p := newTestPayment(t, CaptureAutomatic)
	a, err := p.StartAttempt("mock", "default", now)
	require.NoError(t, err)
	assert.Equal(t, 1, a.AttemptNo)
	assert.Equal(t, AttemptPending, a.Status)
	assert.True(t, ids.HasPrefix(a.PublicID(), ids.PrefixAttempt))
	_, err = p.StartAttempt("mock", "default", now)
	require.ErrorIs(t, err, ErrOperationInProgress)

	a.MarkApproved("mock_ref_1", now.Add(120*time.Millisecond))
	assert.Equal(t, 120, *a.LatencyMs)
	evAuth, err := p.Authorize(AuthorizeParams{Provider: "mock", ProviderReference: "mock_ref_1", Details: &PaymentMethodDetails{Brand: "visa", Last4: "4242"}}, now)
	require.NoError(t, err)
	assert.Equal(t, EventPaymentAuthorized, evAuth.Type)
	assert.Equal(t, 2, evAuth.Seq)
	assert.Equal(t, StatusCreated, *evAuth.FromStatus)
	assert.Equal(t, StatusAuthorized, p.Status)
	assert.Equal(t, money.MustNew(1000, "TWD"), p.AmountAuthorized)
	assert.Equal(t, "visa", p.PaymentMethodDetails.Brand)
	assert.Equal(t, "mock", p.PaymentMethodDetails.TokenProvider)
	assert.Nil(t, p.ExpiresAt)
	assert.Equal(t, now.Add(DefaultAuthValidity), *p.AuthExpiresAt)

	evCap, err := p.Capture(money.MustNew(1000, "TWD"), 59, now)
	require.NoError(t, err)
	assert.Equal(t, EventPaymentCaptured, evCap.Type)
	assert.Equal(t, 3, evCap.Seq)
	assert.Equal(t, StatusCaptured, p.Status)
	assert.Equal(t, 3, p.Version)
	assert.Equal(t, int64(59), evCap.Payload["fee"])
	assert.Equal(t, money.Zero("TWD"), p.RemainingAuthorized())
	assert.Equal(t, a, p.WinningAttempt())
	assert.Equal(t, a, p.CurrentAttempt())
	assert.Nil(t, p.OpenAttempt())

	// 已 captured 不可再 capture / void / start attempt。
	_, err = p.Capture(money.MustNew(1, "TWD"), 0, now)
	require.ErrorIs(t, err, ErrInvalidTransition)
	_, err = p.Void(VoidReasonMerchantRequest, "", now)
	require.ErrorIs(t, err, ErrVoidNotAllowed)
	_, err = p.StartAttempt("mock", "x", now)
	require.ErrorIs(t, err, ErrInvalidTransition)
}

func TestManualCapturePartialAndGuards(t *testing.T) {
	p := newTestPayment(t, CaptureManual)
	_, err := p.StartAttempt("mock", "default", now)
	require.NoError(t, err)
	p.CurrentAttempt().MarkApproved("ref", now)
	_, err = p.Authorize(AuthorizeParams{ProviderReference: "ref", AuthExpiresAt: now.Add(time.Hour)}, now)
	require.NoError(t, err)

	_, err = p.Capture(money.MustNew(1001, "TWD"), 0, now)
	require.ErrorIs(t, err, ErrAmountExceedsAuthorized)
	_, err = p.Capture(money.MustNew(10, "USD"), 0, now)
	require.ErrorIs(t, err, ErrCurrencyMismatch)
	_, err = p.Capture(money.Zero("TWD"), 0, now)
	require.ErrorIs(t, err, ErrAmountTooSmall)
	_, err = p.Capture(money.MustNew(600, "TWD"), 0, now.Add(2*time.Hour))
	require.ErrorIs(t, err, ErrPaymentExpired)

	ev, err := p.Capture(money.MustNew(600, "TWD"), 0, now)
	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, p.Status)
	assert.Equal(t, money.MustNew(600, "TWD"), p.AmountCaptured)
	assert.Equal(t, money.MustNew(400, "TWD"), p.RemainingAuthorized())
	assert.Equal(t, int64(600), ev.Payload["amount"])
}

func TestVoid(t *testing.T) {
	p := newTestPayment(t, CaptureManual)
	_ = must(p.StartAttempt("mock", "default", now))
	p.CurrentAttempt().MarkApproved("ref", now)
	_, err := p.Authorize(AuthorizeParams{ProviderReference: "ref"}, now)
	require.NoError(t, err)
	ev, err := p.Void("", "requested_by_customer", now)
	require.NoError(t, err)
	assert.Equal(t, StatusVoided, p.Status)
	assert.Equal(t, VoidReasonMerchantRequest, *p.VoidReason)
	assert.Equal(t, "merchant_request", ev.Payload["reason"])
	assert.Equal(t, now, *p.VoidedAt)
	assert.True(t, p.Status.IsTerminal())

	// requires_action 也可 void（T10）；created 不行（無 T）。
	p2 := newTestPayment(t, CaptureAutomatic)
	_, err = p2.Void(VoidReasonMerchantRequest, "", now)
	require.ErrorIs(t, err, ErrInvalidTransition)
	_ = must(p2.StartAttempt("mock", "default", now))
	_, err = p2.RequireAction("ref", time.Time{}, now)
	require.NoError(t, err)
	assert.Equal(t, now.Add(DefaultActionWindow), *p2.ExpiresAt)
	_, err = p2.Void(VoidReasonMerchantRequest, "", now)
	require.NoError(t, err)

	// 授權逾期 → voided(authorization_expired)（T13）。
	p3 := newTestPayment(t, CaptureManual)
	_ = must(p3.StartAttempt("mock", "default", now))
	_ = must(p3.Authorize(AuthorizeParams{ProviderReference: "r"}, now))
	_, err = p3.Void(VoidReasonAuthorizationExpired, "", now.Add(8*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, VoidReasonAuthorizationExpired, *p3.VoidReason)
}

func TestFailAndExpire(t *testing.T) {
	p := newTestPayment(t, CaptureAutomatic)
	a := must(p.StartAttempt("mock", "default", now))
	a.MarkFailed(CategoryDeclinedHard, "insufficient_funds", "no funds", "", now)
	assert.Equal(t, AttemptDeclined, a.Status)
	assert.False(t, a.CanFailover())
	ev, err := p.Fail(Failure{Category: CategoryDeclinedHard, Code: "insufficient_funds", Message: "no funds"}, now)
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, p.Status)
	assert.Equal(t, "mock", p.Failure.Provider)
	assert.True(t, p.Failure.Retryable)
	assert.Equal(t, 1, ev.Payload["attempt_count"])
	_, err = p.Fail(Failure{}, now)
	require.ErrorIs(t, err, ErrInvalidTransition)

	// unavailable → 可 failover；requires_action attempt 不可。
	p2 := newTestPayment(t, CaptureAutomatic)
	a2 := must(p2.StartAttempt("mock", "default", now))
	a2.MarkFailed(CategoryProviderUnavailable, "503", "down", "", now)
	assert.Equal(t, AttemptUnavailable, a2.Status)
	assert.True(t, a2.CanFailover())
	assert.Nil(t, p2.OpenAttempt())
	a3, err := p2.StartAttempt("stripe", "fallback", now)
	require.NoError(t, err)
	assert.Equal(t, 2, a3.AttemptNo)
	a3.MarkFailed(CategoryDeclinedSoft, "try_again_later", "", "", now)
	assert.True(t, a3.CanFailover())
	a3.MarkFailed(CategoryDeclinedSoft, "velocity_exceeded", "", "", now)
	assert.False(t, a3.CanFailover())
	a3.MarkFailed(CategoryProviderTimeout, "", "", "", now)
	assert.Equal(t, AttemptUnknown, a3.Status)
	assert.False(t, a3.CanFailover())
	a3.Resolve(AttemptUnavailable, "", now)
	assert.True(t, a3.CanFailover())
	a3.MarkFailed(CategoryAuthenticationRequired, "", "", "", now)
	assert.Equal(t, AttemptDeclined, a3.Status, "authentication_required via MarkFailed degrades to declined")

	// expire：created 與 requires_action 可；authorized 不可。
	p4 := newTestPayment(t, CaptureAutomatic)
	ev, err = p4.Expire(now)
	require.NoError(t, err)
	assert.Equal(t, StatusExpired, p4.Status)
	assert.Equal(t, "created", ev.Payload["previous_status"])
	p5 := newTestPayment(t, CaptureManual)
	_ = must(p5.StartAttempt("mock", "d", now))
	_ = must(p5.Authorize(AuthorizeParams{ProviderReference: "r"}, now))
	_, err = p5.Expire(now)
	require.ErrorIs(t, err, ErrInvalidTransition)
}

func TestRequiresActionThenAuthorize(t *testing.T) {
	p := newTestPayment(t, CaptureAutomatic)
	a := must(p.StartAttempt("mock", "default", now))
	na := &NextAction{Type: "redirect", URL: "http://mock/3ds/x", ExpiresAt: now.Add(10 * time.Minute)}
	a.MarkRequiresAction("ref", na, now)
	assert.Equal(t, AttemptRequiresAction, a.Status)
	assert.Equal(t, a, p.OpenAttempt())
	ev, err := p.RequireAction("ref", na.ExpiresAt, now)
	require.NoError(t, err)
	assert.Equal(t, StatusRequiresAction, p.Status)
	assert.Equal(t, EventPaymentRequiresAction, ev.Type)

	// 逾時後到達的成功結果被拒絕。
	_, err = p.Authorize(AuthorizeParams{ProviderReference: "ref"}, now.Add(time.Hour))
	require.ErrorIs(t, err, ErrPaymentExpired)
	_, err = p.Authorize(AuthorizeParams{ProviderReference: "ref"}, now.Add(time.Minute))
	require.NoError(t, err)
	_, err = p.Capture(p.Amount, 0, now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, p.Status)
}

func TestRefundLifecycle(t *testing.T) {
	p := newTestPayment(t, CaptureAutomatic)
	_ = must(p.StartAttempt("mock", "d", now))
	p.CurrentAttempt().MarkApproved("ref", now)
	_ = must(p.Authorize(AuthorizeParams{Provider: "mock", ProviderReference: "ref"}, now))

	// 未 capture 不可退款。
	r0, err := NewRefund(p, "rk0", money.MustNew(100, "TWD"), "", nil, now)
	require.NoError(t, err)
	_, err = p.ReserveRefund(r0, now)
	require.ErrorIs(t, err, ErrPaymentNotRefundable)

	_ = must(p.Capture(p.Amount, 0, now))
	versionBefore := p.Version

	_, err = NewRefund(p, "rk", money.MustNew(100, "TWD"), "because", nil, now)
	require.ErrorIs(t, err, ErrInvalidTransition)

	r1, err := NewRefund(p, "rk1", money.MustNew(600, "TWD"), "requested_by_customer", map[string]string{"a": "b"}, now)
	require.NoError(t, err)
	assert.True(t, ids.HasPrefix(r1.PublicID, ids.PrefixRefund))
	assert.Equal(t, "mock", r1.Provider)
	ev, err := p.ReserveRefund(r1, now)
	require.NoError(t, err)
	assert.Equal(t, EventRefundCreated, ev.Type)
	assert.Equal(t, StatusCaptured, p.Status, "reserve does not change status")
	assert.Equal(t, versionBefore+1, p.Version)
	assert.Equal(t, money.MustNew(600, "TWD"), p.AmountRefundPending)
	assert.Equal(t, money.MustNew(400, "TWD"), p.AvailableToRefund())

	// 併發第二筆超額（只剩 400 可退）。
	r2 := must(NewRefund(p, "rk2", money.MustNew(500, "TWD"), "", nil, now))
	_, err = p.ReserveRefund(r2, now)
	require.ErrorIs(t, err, ErrRefundExceedsAvailable)
	r2b := must(NewRefund(p, "rk2", money.MustNew(10, "USD"), "", nil, now))
	_, err = p.ReserveRefund(r2b, now)
	require.ErrorIs(t, err, ErrCurrencyMismatch)
	r2c := must(NewRefund(p, "rk2", money.Zero("TWD"), "", nil, now))
	_, err = p.ReserveRefund(r2c, now)
	require.ErrorIs(t, err, ErrAmountTooSmall)

	// 第一筆成功 → partially_refunded。
	require.NoError(t, r1.Succeed("mock_re_1", now))
	ev, err = p.MarkRefunded(r1, now)
	require.NoError(t, err)
	assert.Equal(t, EventRefundSucceeded, ev.Type)
	assert.Equal(t, StatusPartiallyRefunded, p.Status)
	assert.Equal(t, money.MustNew(600, "TWD"), p.AmountRefunded)
	assert.True(t, p.AmountRefundPending.IsZero())
	require.ErrorIs(t, r1.Succeed("x", now), ErrInvalidTransition)

	// 第二筆失敗 → 釋放保留額。
	r3 := must(NewRefund(p, "rk3", money.MustNew(400, "TWD"), "duplicate", nil, now))
	_, err = p.ReserveRefund(r3, now)
	require.NoError(t, err)
	require.NoError(t, r3.Fail("provider_unavailable", "down", now))
	ev, err = p.ReleaseRefund(r3, now)
	require.NoError(t, err)
	assert.Equal(t, EventRefundFailed, ev.Type)
	assert.Equal(t, StatusPartiallyRefunded, p.Status)
	assert.Equal(t, money.MustNew(400, "TWD"), p.AvailableToRefund())
	require.ErrorIs(t, r3.Fail("x", "", now), ErrInvalidTransition)

	// 剩餘全退 → refunded；之後不可再退。
	r4 := must(NewRefund(p, "rk4", money.MustNew(400, "TWD"), "other", nil, now))
	_, err = p.ReserveRefund(r4, now)
	require.NoError(t, err)
	require.NoError(t, r4.Succeed("mock_re_4", now))
	_, err = p.MarkRefunded(r4, now)
	require.NoError(t, err)
	assert.Equal(t, StatusRefunded, p.Status)
	r5 := must(NewRefund(p, "rk5", money.MustNew(1, "TWD"), "", nil, now))
	_, err = p.ReserveRefund(r5, now)
	require.ErrorIs(t, err, ErrPaymentNotRefundable)

	// disputed 回專屬錯誤。
	p.Status = StatusDisputed
	require.ErrorIs(t, p.CanRefund(), ErrPaymentDisputed)
	p.Status = StatusChargebackWon
	require.NoError(t, p.CanRefund())
}

func TestTransitionErrorAndProviderError(t *testing.T) {
	err := TransitionError(StatusCaptured, StatusVoided)
	require.ErrorIs(t, err, ErrInvalidTransition)
	assert.Contains(t, err.Message, "captured")
	assert.Equal(t, 409, err.HTTPStatus())

	pe := ProviderError(CategoryDeclinedHard, "insufficient_funds", "declined")
	assert.Equal(t, "card_declined", pe.Code)
	assert.Equal(t, 402, pe.HTTPStatus())
	assert.Contains(t, pe.Message, "insufficient_funds")
	pe = ProviderError(CategoryProviderUnavailable, "", "")
	assert.Equal(t, "provider_unavailable", pe.Code)
	assert.Equal(t, 503, pe.HTTPStatus())
}

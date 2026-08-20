package providermock

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
)

func newSvc() *Service {
	return NewService(Config{WebhookSecret: "whsec_test", BaseURL: "http://mock:9101", Version: "test"})
}

func authReq(token, paymentID, idem string, capture bool) *providerv1.AuthorizeRequest {
	return &providerv1.AuthorizeRequest{
		PaymentId: paymentID, MerchantId: "m", IdempotencyKey: idem,
		Amount: &commonv1.Money{AmountMinor: 1000, Currency: "TWD"}, CaptureImmediately: capture,
		Instrument: &providerv1.PaymentInstrument{Instrument: &providerv1.PaymentInstrument_CardToken{CardToken: &providerv1.CardToken{Token: token, TokenProvider: "mock"}}},
	}
}

func TestAuthorizeScenarios(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		capture    bool
		wantStatus providerv1.AuthorizationStatus
		wantCat    providerv1.ProviderErrorCategory
		wantCode   string
		wantGRPC   codes.Code
	}{
		{"ok automatic", TokOK, true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED, 0, "", codes.OK},
		{"ok manual", TokOK, false, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_AUTHORIZED, 0, "", codes.OK},
		{"unknown token behaves like ok", "tok_whatever", true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED, 0, "", codes.OK},
		{"hard decline", TokDeclineHard, true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD, "stolen_card", codes.OK},
		{"soft decline", TokDeclineSoft, true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_SOFT, "try_again_later", codes.OK},
		{"insufficient funds", TokInsufficientFunds, true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD, "insufficient_funds", codes.OK},
		{"fraud", TokFraud, true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_FRAUD_SUSPECTED, "fraudulent", codes.OK},
		{"invalid", TokInvalid, true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "currency_not_supported", codes.OK},
		{"3ds", Tok3DS, true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_REQUIRES_ACTION, 0, "", codes.OK},
		{"unavailable", TokUnavailable, true, 0, 0, "", codes.Unavailable},
		{"rate limited", TokRateLimited, true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_PROVIDER_UNAVAILABLE, "rate_limited", codes.OK},
		{"slow", "tok_slow_20", true, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED, 0, "", codes.OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSvc()
			resp, err := s.Authorize(context.Background(), authReq(tt.token, "pay_1", "att_1", tt.capture))
			if tt.wantGRPC != codes.OK {
				require.Error(t, err)
				assert.Equal(t, tt.wantGRPC, status.Code(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, resp.GetStatus())
			if tt.wantCat != 0 {
				assert.False(t, resp.GetResult().GetSuccess())
				assert.Equal(t, tt.wantCat, resp.GetResult().GetErrorCategory())
				assert.Equal(t, tt.wantCode, resp.GetResult().GetProviderErrorCode())
				assert.Equal(t, 0, s.Store().Len())
				return
			}
			assert.True(t, resp.GetResult().GetSuccess())
			assert.NotEmpty(t, resp.GetResult().GetProviderReference())
			assert.Equal(t, int64(59), resp.GetFee().GetAmountMinor(), "290bps of 1000 + 30")
			if tt.wantStatus == providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_REQUIRES_ACTION {
				assert.Contains(t, resp.GetNextAction().GetRedirect().GetUrl(), "http://mock:9101/3ds/")
			}
		})
	}
}

func TestTimeoutScenario(t *testing.T) {
	s := newSvc()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := s.Authorize(ctx, authReq(TokTimeout, "pay_t", "att_t", true))
	require.Error(t, err)
	assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
	st, err := s.GetPaymentStatus(context.Background(), &providerv1.GetPaymentStatusRequest{ProviderReference: "nope"})
	require.NoError(t, err)
	assert.Equal(t, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_NOT_FOUND, st.GetStatus())
}

func TestUnavailableOnceAndIdempotency(t *testing.T) {
	s := newSvc()
	_, err := s.Authorize(context.Background(), authReq(TokUnavailableOnce, "pay_u", "att_1", true))
	require.Equal(t, codes.Unavailable, status.Code(err))
	resp, err := s.Authorize(context.Background(), authReq(TokUnavailableOnce, "pay_u", "att_2", true))
	require.NoError(t, err)
	assert.True(t, resp.GetResult().GetSuccess())
	// 另一筆付款第一次仍 unavailable（以 payment_id 計數）。
	_, err = s.Authorize(context.Background(), authReq(TokUnavailableOnce, "pay_v", "att_3", true))
	require.Equal(t, codes.Unavailable, status.Code(err))

	// 同 idempotency key 重送回同一 reference，不重複建立。
	r1 := must(s.Authorize(context.Background(), authReq(TokOK, "pay_i", "att_same", true)))
	r2 := must(s.Authorize(context.Background(), authReq(TokOK, "pay_i", "att_same", true)))
	assert.Equal(t, r1.GetResult().GetProviderReference(), r2.GetResult().GetProviderReference())
	assert.Equal(t, 2, s.Store().Len(), "pay_u (second try) + pay_i; pay_v never succeeded")
}

func TestCaptureVoidRefundLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newSvc()
	auth, err := s.Authorize(ctx, authReq(TokOK, "pay_l", "att_l", false))
	require.NoError(t, err)
	ref := auth.GetResult().GetProviderReference()

	// 部分 capture。
	capResp, err := s.Capture(ctx, &providerv1.CaptureRequest{ProviderReference: ref, Amount: &commonv1.Money{AmountMinor: 600, Currency: "TWD"}, IdempotencyKey: "c1"})
	require.NoError(t, err)
	assert.True(t, capResp.GetResult().GetSuccess())
	assert.Equal(t, int64(600), capResp.GetCapturedAmount().GetAmountMinor())
	// 冪等：再 capture 回既有結果。
	cap2 := must(s.Capture(ctx, &providerv1.CaptureRequest{ProviderReference: ref}))
	assert.True(t, cap2.GetResult().GetSuccess())
	assert.Equal(t, int64(600), cap2.GetCapturedAmount().GetAmountMinor())
	// 已 capture 不可 void。
	v := must(s.Void(ctx, &providerv1.VoidRequest{ProviderReference: ref}))
	assert.False(t, v.GetResult().GetSuccess())
	assert.Equal(t, "invalid_state", v.GetResult().GetProviderErrorCode())

	// 退款：部分 → 冪等 → 超額 → 全額。
	r1 := must(s.Refund(ctx, &providerv1.RefundRequest{ProviderReference: ref, RefundId: "re_1", Amount: &commonv1.Money{AmountMinor: 200, Currency: "TWD"}}))
	assert.Equal(t, providerv1.RefundState_REFUND_STATE_SUCCEEDED, r1.GetStatus())
	r1b := must(s.Refund(ctx, &providerv1.RefundRequest{ProviderReference: ref, RefundId: "re_1", Amount: &commonv1.Money{AmountMinor: 200, Currency: "TWD"}}))
	assert.Equal(t, r1.GetProviderRefundReference(), r1b.GetProviderRefundReference())
	r2 := must(s.Refund(ctx, &providerv1.RefundRequest{ProviderReference: ref, RefundId: "re_2", Amount: &commonv1.Money{AmountMinor: 500, Currency: "TWD"}}))
	assert.Equal(t, providerv1.RefundState_REFUND_STATE_FAILED, r2.GetStatus())
	assert.Equal(t, "amount_too_large", r2.GetResult().GetProviderErrorCode())
	r3 := must(s.Refund(ctx, &providerv1.RefundRequest{ProviderReference: ref, RefundId: "re_3", Amount: &commonv1.Money{AmountMinor: 400, Currency: "TWD"}}))
	assert.Equal(t, providerv1.RefundState_REFUND_STATE_SUCCEEDED, r3.GetStatus())
	st := must(s.GetPaymentStatus(ctx, &providerv1.GetPaymentStatusRequest{ProviderReference: ref}))
	assert.Equal(t, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_REFUNDED, st.GetStatus())
	assert.Equal(t, int64(600), st.GetRefundedAmount().GetAmountMinor())

	// void 流程。
	auth2 := must(s.Authorize(ctx, authReq(TokOK, "pay_v", "att_v", false)))
	ref2 := auth2.GetResult().GetProviderReference()
	v2 := must(s.Void(ctx, &providerv1.VoidRequest{ProviderReference: ref2}))
	assert.True(t, v2.GetResult().GetSuccess())
	v3 := must(s.Void(ctx, &providerv1.VoidRequest{ProviderReference: ref2}))
	assert.True(t, v3.GetResult().GetSuccess(), "void is idempotent")
	c3 := must(s.Capture(ctx, &providerv1.CaptureRequest{ProviderReference: ref2}))
	assert.False(t, c3.GetResult().GetSuccess())

	// 未知 reference。
	c4 := must(s.Capture(ctx, &providerv1.CaptureRequest{ProviderReference: "nope"}))
	assert.Equal(t, "not_found", c4.GetResult().GetProviderErrorCode())

	// tok_capture_fail / tok_refund_fail。
	a5 := must(s.Authorize(ctx, authReq(TokCaptureFail, "pay_cf", "att_cf", false)))
	_, err = s.Capture(ctx, &providerv1.CaptureRequest{ProviderReference: a5.GetResult().GetProviderReference()})
	assert.Equal(t, codes.Unavailable, status.Code(err))
	a6 := must(s.Authorize(ctx, authReq(TokRefundFail, "pay_rf", "att_rf", true)))
	r6 := must(s.Refund(ctx, &providerv1.RefundRequest{ProviderReference: a6.GetResult().GetProviderReference(), RefundId: "re_6", Amount: &commonv1.Money{AmountMinor: 1, Currency: "TWD"}}))
	assert.Equal(t, providerv1.RefundState_REFUND_STATE_FAILED, r6.GetStatus())
}

func Test3DSConfirmViaGetPaymentStatus(t *testing.T) {
	ctx := context.Background()
	s := newSvc()
	auth := must(s.Authorize(ctx, authReq(Tok3DS, "pay_3", "att_3", true)))
	ref := auth.GetResult().GetProviderReference()
	st := must(s.GetPaymentStatus(ctx, &providerv1.GetPaymentStatusRequest{ProviderReference: ref}))
	assert.Equal(t, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_AUTHORIZED, st.GetStatus())
	capResp := must(s.Capture(ctx, &providerv1.CaptureRequest{ProviderReference: ref}))
	assert.True(t, capResp.GetResult().GetSuccess())
}

func TestParseWebhook(t *testing.T) {
	s := newSvc()
	auth := must(s.Authorize(context.Background(), authReq(TokOK, "pay_w", "att_w", true)))
	ref := auth.GetResult().GetProviderReference()
	body := must(json.Marshal(map[string]any{"id": "evt_1", "type": "refund.succeeded", "provider_reference": ref, "refund_id": "re_9", "amount_minor": 100, "currency": "TWD", "occurred_at": "2026-08-20T10:00:00Z"}))
	ts := time.Now().Unix()
	hdr := func(v string) map[string]*providerv1.HeaderValues {
		return map[string]*providerv1.HeaderValues{"x-mock-signature": {Values: []string{v}}}
	}

	resp, err := s.ParseWebhook(context.Background(), &providerv1.ParseWebhookRequest{Provider: "mock", Headers: hdr(s.SignWebhook(ts, body)), Body: body})
	require.NoError(t, err)
	assert.True(t, resp.GetVerified())
	assert.False(t, resp.GetIgnored())
	assert.Equal(t, providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_REFUND_SUCCEEDED, resp.GetEvent().GetEventType())
	assert.Equal(t, "pay_w", resp.GetEvent().GetPaymentId(), "payment id resolved from store")
	assert.Equal(t, int64(100), resp.GetEvent().GetAmount().GetAmountMinor())
	assert.Equal(t, "re_9", resp.GetEvent().GetRefundId())

	bad := must(s.ParseWebhook(context.Background(), &providerv1.ParseWebhookRequest{Headers: hdr("t=1,v1=00"), Body: body}))
	assert.False(t, bad.GetVerified())
	missing := must(s.ParseWebhook(context.Background(), &providerv1.ParseWebhookRequest{Body: body}))
	assert.False(t, missing.GetVerified())
	tampered := must(s.ParseWebhook(context.Background(), &providerv1.ParseWebhookRequest{Headers: hdr(s.SignWebhook(ts, body)), Body: []byte(`{"type":"x"}`)}))
	assert.False(t, tampered.GetVerified())

	unknown := []byte(`{"id":"evt_2","type":"something.new","provider_reference":"x"}`)
	u := must(s.ParseWebhook(context.Background(), &providerv1.ParseWebhookRequest{Headers: hdr(s.SignWebhook(ts, unknown)), Body: unknown}))
	assert.True(t, u.GetVerified())
	assert.True(t, u.GetIgnored())

	malformed := []byte(`{not json`)
	m := must(s.ParseWebhook(context.Background(), &providerv1.ParseWebhookRequest{Headers: hdr(s.SignWebhook(ts, malformed)), Body: malformed}))
	assert.True(t, m.GetVerified())
	assert.True(t, m.GetIgnored())
}

func TestHealthCheck(t *testing.T) {
	s := newSvc()
	resp, err := s.HealthCheck(context.Background(), &providerv1.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, providerv1.HealthStatus_HEALTH_STATUS_SERVING, resp.GetStatus())
	assert.Equal(t, "mock", resp.GetProvider())
	assert.Contains(t, resp.GetCapabilities().GetCurrencies(), "TWD")
	assert.Positive(t, s.Calls())
}

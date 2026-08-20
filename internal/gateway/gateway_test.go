package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	merchantv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/merchant/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/idempotency"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

const (
	devKey    = "pk_test_dev_0000000000000000"
	devSecret = "sk_test_dev_secret_change_me"
	devMch    = "0190a1b2-c3d4-7e5f-8a9b-000000000001"
)

// fakePayments 實作 paymentv1.PaymentServiceClient（只實作需要的方法）。
type fakePayments struct {
	paymentv1.PaymentServiceClient
	mu       sync.Mutex
	created  []*paymentv1.CreatePaymentRequest
	createFn func(*paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error)
}

func samplePayment(id string) *paymentv1.Payment {
	st := paymentv1.PaymentStatus_PAYMENT_STATUS_CAPTURED
	return &paymentv1.Payment{
		Id: id, MerchantId: devMch, Amount: &commonv1.Money{AmountMinor: 1000, Currency: "TWD"},
		CapturedAmount: &commonv1.Money{AmountMinor: 1000, Currency: "TWD"}, RefundedAmount: &commonv1.Money{AmountMinor: 0, Currency: "TWD"},
		Status: st, CaptureMethod: paymentv1.CaptureMethod_CAPTURE_METHOD_AUTOMATIC, PaymentMethodType: paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_CARD,
		PaymentMethodDetails: &paymentv1.PaymentMethodDetails{Brand: "visa", Last4: "4242"}, Provider: "mock", ProviderReference: "mock_pi_1",
		Customer:  &paymentv1.Customer{Id: "cus_1", IpAddress: "1.2.3.4"},
		Attempts:  []*paymentv1.PaymentAttempt{{Id: "att_1", Sequence: 1, Provider: "mock", Status: paymentv1.AttemptStatus_ATTEMPT_STATUS_APPROVED, RoutingReason: "default", CreatedAt: timestamppb.Now()}},
		CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now(), Metadata: map[string]string{"order": "1"},
	}
}

func (f *fakePayments) CreatePayment(_ context.Context, req *paymentv1.CreatePaymentRequest, _ ...grpc.CallOption) (*paymentv1.CreatePaymentResponse, error) {
	f.mu.Lock()
	f.created = append(f.created, req)
	f.mu.Unlock()
	if f.createFn != nil {
		return f.createFn(req)
	}
	return &paymentv1.CreatePaymentResponse{Payment: samplePayment("pay_1")}, nil
}

func (f *fakePayments) GetPayment(_ context.Context, req *paymentv1.GetPaymentRequest, _ ...grpc.CallOption) (*paymentv1.GetPaymentResponse, error) {
	if req.GetPaymentId() == "pay_missing" {
		return nil, grpcx.ErrorFromDomain(apperr.ErrResourceMissing)
	}
	return &paymentv1.GetPaymentResponse{Payment: samplePayment(req.GetPaymentId())}, nil
}

func (f *fakePayments) CapturePayment(_ context.Context, req *paymentv1.CapturePaymentRequest, _ ...grpc.CallOption) (*paymentv1.CapturePaymentResponse, error) {
	if req.GetAmount().GetAmountMinor() > 1000 {
		return nil, grpcx.ErrorFromDomain(apperr.New(apperr.TypeInvalidRequest, "capture_amount_exceeds_authorized", "too much").WithParam("amount"))
	}
	return &paymentv1.CapturePaymentResponse{Payment: samplePayment(req.GetPaymentId())}, nil
}

func (f *fakePayments) CreateRefund(_ context.Context, req *paymentv1.CreateRefundRequest, _ ...grpc.CallOption) (*paymentv1.CreateRefundResponse, error) {
	return &paymentv1.CreateRefundResponse{Refund: &paymentv1.Refund{Id: "re_1", PaymentId: req.GetPaymentId(), Amount: req.GetAmount(), Status: paymentv1.RefundStatus_REFUND_STATUS_SUCCEEDED, CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now()}}, nil
}

func (f *fakePayments) ListPayments(context.Context, *paymentv1.ListPaymentsRequest, ...grpc.CallOption) (*paymentv1.ListPaymentsResponse, error) {
	return &paymentv1.ListPaymentsResponse{Payments: []*paymentv1.Payment{samplePayment("pay_1")}, Page: &commonv1.PageResponse{NextPageToken: "abc", HasMore: true}}, nil
}

type fakeProviderAdapter struct {
	providerv1.ProviderAdapterClient
	verified bool
}

func (f *fakeProviderAdapter) ParseWebhook(context.Context, *providerv1.ParseWebhookRequest, ...grpc.CallOption) (*providerv1.ParseWebhookResponse, error) {
	return &providerv1.ParseWebhookResponse{Verified: f.verified, RejectionReason: "bad sig", Event: &providerv1.ProviderWebhookEvent{ProviderEventId: "evt_1", EventType: providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_CAPTURE_SUCCEEDED}}, nil
}

type testEnv struct {
	srv      *httptest.Server
	payments *fakePayments
	verifier KeyVerifier
	now      time.Time
}

func newEnv(t *testing.T, verifier KeyVerifier, rps ...int) *testEnv {
	t.Helper()
	limit := 1000
	if len(rps) > 0 {
		limit = rps[0]
	}
	fp := &fakePayments{}
	now := time.Now()
	env := &testEnv{payments: fp, verifier: verifier, now: now}
	gw := New(Deps{
		Payments: fp, Providers: map[string]providerv1.ProviderAdapterClient{"mock": &fakeProviderAdapter{verified: true}, "bad": &fakeProviderAdapter{}},
		Verifier: verifier, Idem: idempotency.NewMemoryStore(), Limiter: NewMemoryRateLimiter(limit), RPS: limit,
		Logger: logx.NewWithWriter(io.Discard, "gw", "dev", "debug"), Clock: func() time.Time { return env.now },
	})
	env.srv = httptest.NewServer(gw.Handler())
	t.Cleanup(env.srv.Close)
	return env
}

type reqOpt func(*http.Request)

func withIdem(k string) reqOpt { return func(r *http.Request) { r.Header.Set("Idempotency-Key", k) } }
func withHeader(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func (e *testEnv) do(t *testing.T, method, target, body, secret string, opts ...reqOpt) (*http.Response, []byte) {
	t.Helper()
	// 每次請求推進 1 秒：canonical 不含 nonce，同一秒內相同請求會產生相同簽章而被重放偵測擋下。
	e.now = e.now.Add(time.Second)
	ts := strconv.FormatInt(e.now.Unix(), 10)
	req, err := http.NewRequest(method, e.srv.URL+target, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+devKey)
	req.Header.Set("X-Timestamp", ts)
	if secret != "" {
		req.Header.Set("X-Signature", sig.Sign(secret, ts, method, target, []byte(body)))
	}
	req.Header.Set("Content-Type", "application/json")
	for _, o := range opts {
		o(req)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b := must(io.ReadAll(resp.Body))
	return resp, b
}

const createBody = `{"amount":{"amount_minor":1000,"currency":"TWD"},"capture_method":"automatic","payment_method":{"type":"card","card":{"token":"tok_ok","token_provider":"mock"}},"customer":{"id":"cus_1"},"metadata":{"order":"1"}}`

func errCode(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var e struct {
		Error struct {
			Type, Code, RequestID string `json:"-"`
			T                     string `json:"type"`
			C                     string `json:"code"`
			R                     string `json:"request_id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e), string(body))
	assert.NotEmpty(t, e.Error.R)
	return e.Error.T, e.Error.C
}

func TestCreatePaymentHappyPathAndReplay(t *testing.T) {
	env := newEnv(t, &DevVerifier{APIKey: devKey, SigningSecret: devSecret, MerchantID: devMch})
	resp, body := env.do(t, http.MethodPost, "/v1/payments", createBody, devSecret, withIdem("idem-1"))
	assert.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"))
	var p map[string]any
	require.NoError(t, json.Unmarshal(body, &p))
	assert.Equal(t, "pay_1", p["id"])
	assert.Equal(t, "payment", p["object"])
	assert.Equal(t, "captured", p["status"])
	assert.InDelta(t, 1000, asMap(t, p["amount"])["amount_minor"], 0)
	assert.Equal(t, "card", asMap(t, p["payment_method"])["type"])
	assert.Nil(t, asMap(t, p["customer"])["ip_address"], "ip masked")
	assert.Equal(t, false, p["livemode"])
	assert.Len(t, p["attempts"], 1)
	attempts, ok := p["attempts"].([]any)
	require.True(t, ok)
	assert.Equal(t, "approved", asMap(t, attempts[0])["status"])

	req := env.payments.created[0]
	assert.Equal(t, devMch, req.GetMerchantId())
	assert.Equal(t, "idem-1", req.GetIdempotencyKey())
	assert.Equal(t, "tok_ok", req.GetPaymentMethod().GetCardToken().GetToken())
	assert.False(t, req.GetLivemode())

	// 同 key 同 payload → 回放（不再呼叫 payment-service）。
	resp2, body2 := env.do(t, http.MethodPost, "/v1/payments", createBody, devSecret, withIdem("idem-1"))
	assert.Equal(t, http.StatusCreated, resp2.StatusCode)
	assert.Equal(t, "true", resp2.Header.Get("Idempotent-Replayed"))
	assert.JSONEq(t, string(body), string(body2))
	assert.Len(t, env.payments.created, 1)

	// 同 key 不同 payload → 409。
	other := strings.Replace(createBody, "1000", "2000", 1)
	resp3, body3 := env.do(t, http.MethodPost, "/v1/payments", other, devSecret, withIdem("idem-1"))
	assert.Equal(t, http.StatusConflict, resp3.StatusCode)
	typ, code := errCode(t, body3)
	assert.Equal(t, "idempotency_error", typ)
	assert.Equal(t, "idempotency_key_payload_mismatch", code)

	// 缺 key → 400。
	resp4, body4 := env.do(t, http.MethodPost, "/v1/payments", createBody, devSecret)
	assert.Equal(t, http.StatusBadRequest, resp4.StatusCode)
	_, code = errCode(t, body4)
	assert.Equal(t, "idempotency_key_missing", code)
	resp5, body5 := env.do(t, http.MethodPost, "/v1/payments", createBody, devSecret, withIdem("bad key with spaces"))
	assert.Equal(t, http.StatusBadRequest, resp5.StatusCode)
	assert.Contains(t, string(body5), "idempotency_key_invalid")
}

func TestIdempotencyInProgressAndNoCacheOn5xx(t *testing.T) {
	env := newEnv(t, &DevVerifier{APIKey: devKey, SigningSecret: devSecret, MerchantID: devMch})
	env.payments.createFn = func(*paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}
	resp, body := env.do(t, http.MethodPost, "/v1/payments", createBody, devSecret, withIdem("k5"))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	_, code := errCode(t, body)
	assert.Equal(t, "service_unavailable", code)
	// 5xx 不快取：恢復後同 key 可成功。
	env.payments.createFn = nil
	resp2, _ := env.do(t, http.MethodPost, "/v1/payments", createBody, devSecret, withIdem("k5"))
	assert.Equal(t, http.StatusCreated, resp2.StatusCode)
	assert.Empty(t, resp2.Header.Get("Idempotent-Replayed"))

	// 4xx 會快取並回放。
	env.payments.createFn = func(*paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error) {
		return nil, grpcx.ErrorFromDomain(apperr.New(apperr.TypeInvalidRequest, "no_route_available", "none"))
	}
	resp3, _ := env.do(t, http.MethodPost, "/v1/payments", createBody, devSecret, withIdem("k6"))
	assert.Equal(t, http.StatusUnprocessableEntity, resp3.StatusCode)
	env.payments.createFn = nil
	resp4, _ := env.do(t, http.MethodPost, "/v1/payments", createBody, devSecret, withIdem("k6"))
	assert.Equal(t, http.StatusUnprocessableEntity, resp4.StatusCode)
	assert.Equal(t, "true", resp4.Header.Get("Idempotent-Replayed"))
}

func TestAuthFailures(t *testing.T) {
	env := newEnv(t, &DevVerifier{APIKey: devKey, SigningSecret: devSecret, MerchantID: devMch})
	tests := []struct {
		name     string
		secret   string
		opts     []reqOpt
		wantCode string
		wantHTTP int
	}{
		{"wrong secret", "other", nil, "signature_invalid", 401},
		{"missing signature", "", nil, "signature_missing", 401},
		{"bad format", "", []reqOpt{withHeader("X-Signature", "deadbeef")}, "signature_missing", 401},
		{"missing authorization", devSecret, []reqOpt{withHeader("Authorization", "")}, "invalid_api_key", 401},
		{"wrong key", devSecret, []reqOpt{withHeader("Authorization", "Bearer pk_test_wrong")}, "invalid_api_key", 401},
		{"basic scheme", devSecret, []reqOpt{withHeader("Authorization", "Basic abc")}, "invalid_api_key", 401},
		{"old timestamp", devSecret, []reqOpt{withHeader("X-Timestamp", strconv.FormatInt(env.now.Add(-10*time.Minute).Unix(), 10))}, "timestamp_out_of_window", 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := env.do(t, http.MethodGet, "/v1/payments/pay_1", "", tt.secret, tt.opts...)
			assert.Equal(t, tt.wantHTTP, resp.StatusCode, string(body))
			typ, code := errCode(t, body)
			assert.Equal(t, "authentication_error", typ)
			assert.Equal(t, tt.wantCode, code)
		})
	}
	// 時間窗：用正確 secret 但舊 timestamp 重新簽 → timestamp_out_of_window。
	old := strconv.FormatInt(env.now.Add(-10*time.Minute).Unix(), 10)
	req := must(http.NewRequest(http.MethodGet, env.srv.URL+"/v1/payments/pay_1", http.NoBody))
	req.Header.Set("Authorization", "Bearer "+devKey)
	req.Header.Set("X-Timestamp", old)
	req.Header.Set("X-Signature", sig.Sign(devSecret, old, "GET", "/v1/payments/pay_1", nil))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	b := must(io.ReadAll(resp.Body))
	_ = resp.Body.Close()
	assert.Equal(t, 401, resp.StatusCode)
	_, code := errCode(t, b)
	assert.Equal(t, "timestamp_out_of_window", code)

	// 重放：同一簽章（同 ts）第二次 → signature_replayed。
	sameTS := strconv.FormatInt(env.now.Unix(), 10)
	sameSig := sig.Sign(devSecret, sameTS, "GET", "/v1/payments/pay_1", nil)
	resp1, _ := env.do(t, http.MethodGet, "/v1/payments/pay_1", "", "", withHeader("X-Timestamp", sameTS), withHeader("X-Signature", sameSig))
	assert.Equal(t, 200, resp1.StatusCode)
	resp2, body2 := env.do(t, http.MethodGet, "/v1/payments/pay_1", "", "", withHeader("X-Timestamp", sameTS), withHeader("X-Signature", sameSig))
	assert.Equal(t, 401, resp2.StatusCode)
	_, code = errCode(t, body2)
	assert.Equal(t, "signature_replayed", code)

	// 簽章綁定 method/target：拿 GET 簽章打另一路徑。
	ts := strconv.FormatInt(env.now.Unix(), 10)
	resp3, body3 := env.do(t, http.MethodGet, "/v1/payments/pay_2", "", "", withHeader("X-Signature", sig.Sign(devSecret, ts, "GET", "/v1/payments/pay_1", nil)))
	assert.Equal(t, 401, resp3.StatusCode)
	_, code = errCode(t, body3)
	assert.Equal(t, "signature_invalid", code)
}

type staticVerifier struct{ p *Principal }

func (s staticVerifier) VerifyKey(context.Context, string) (*Principal, error) { return s.p, nil }

func TestScopesMerchantStatusAndPreviousSecret(t *testing.T) {
	p := &Principal{MerchantID: devMch, APIKeyID: "key_1", Scopes: []string{"payments:read"}, MerchantStatus: merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED, SigningSecret: "new_secret", PreviousSigningSecret: devSecret}
	env := newEnv(t, staticVerifier{p})
	// 舊 secret 仍可驗（輪替期）。
	resp, _ := env.do(t, http.MethodGet, "/v1/payments/pay_1", "", devSecret)
	assert.Equal(t, 200, resp.StatusCode)
	// suspended：建立付款 403。
	resp, body := env.do(t, http.MethodPost, "/v1/payments", createBody, "new_secret", withIdem("s1"))
	assert.Equal(t, 403, resp.StatusCode)
	_, code := errCode(t, body)
	assert.Equal(t, "merchant_suspended", code)
	// scope 不足：refund write。
	resp, body = env.do(t, http.MethodPost, "/v1/refunds", `{"payment_id":"pay_1"}`, "new_secret", withIdem("s2"))
	assert.Equal(t, 403, resp.StatusCode)
	_, code = errCode(t, body)
	assert.Equal(t, "insufficient_permissions", code)

	p.MerchantStatus = merchantv1.MerchantStatus_MERCHANT_STATUS_CLOSED
	p.Scopes = nil
	resp, body = env.do(t, http.MethodPost, "/v1/refunds", `{"payment_id":"pay_1"}`, "new_secret", withIdem("s3"))
	assert.Equal(t, 403, resp.StatusCode)
	_, code = errCode(t, body)
	assert.Equal(t, "merchant_closed", code)
	resp, _ = env.do(t, http.MethodGet, "/v1/payments/pay_1", "", "new_secret")
	assert.Equal(t, 200, resp.StatusCode, "closed merchants may still read")
}

func TestOtherEndpointsAndErrors(t *testing.T) {
	env := newEnv(t, &DevVerifier{APIKey: devKey, SigningSecret: devSecret, MerchantID: devMch})
	resp, body := env.do(t, http.MethodGet, "/v1/payments/pay_missing", "", devSecret)
	assert.Equal(t, 404, resp.StatusCode)
	_, code := errCode(t, body)
	assert.Equal(t, "resource_missing", code)

	resp, body = env.do(t, http.MethodPost, "/v1/payments/pay_1/capture", `{"amount":{"amount_minor":5000,"currency":"TWD"}}`, devSecret, withIdem("c1"))
	assert.Equal(t, 422, resp.StatusCode)
	_, code = errCode(t, body)
	assert.Equal(t, "capture_amount_exceeds_authorized", code)
	resp, _ = env.do(t, http.MethodPost, "/v1/payments/pay_1/capture", `{}`, devSecret, withIdem("c2"))
	assert.Equal(t, 200, resp.StatusCode)

	resp, body = env.do(t, http.MethodPost, "/v1/refunds", `{"payment_id":"pay_1","amount":{"amount_minor":100,"currency":"TWD"},"reason":"duplicate"}`, devSecret, withIdem("r1"))
	assert.Equal(t, 201, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"object":"refund"`)

	resp, body = env.do(t, http.MethodPost, "/v1/refunds", `{}`, devSecret, withIdem("r2"))
	assert.Equal(t, 400, resp.StatusCode)
	_, code = errCode(t, body)
	assert.Equal(t, "parameter_missing", code)

	resp, body = env.do(t, http.MethodGet, "/v1/payments?limit=5&status=captured,failed", "", devSecret)
	assert.Equal(t, 200, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"has_more":true`)

	// 驗證錯誤。
	for i, tc := range []struct {
		body, code string
		status     int
	}{
		{`{"payment_method":{"type":"card","card":{"token":"tok","token_provider":"mock"}}}`, "parameter_missing", 400},
		{`{"amount":{"amount_minor":100,"currency":"XXX"},"payment_method":{"type":"card","card":{"token":"tok","token_provider":"mock"}}}`, "currency_not_supported", 422},
		{`{"amount":{"amount_minor":100,"currency":"TWD"},"payment_method":{"type":"card","card":{"token":"4242424242424242","token_provider":"mock"}}}`, "pan_not_allowed", 400},
		{`{"amount":{"amount_minor":100,"currency":"TWD"},"payment_method":{"type":"crypto"}}`, "parameter_invalid", 400},
		{`{"amount":{"amount_minor":100,"currency":"TWD"},"capture_method":"later","payment_method":{"type":"card","card":{"token":"tok","token_provider":"mock"}}}`, "parameter_invalid", 400},
		{`{bad json`, "parameter_invalid", 400},
	} {
		resp, body = env.do(t, http.MethodPost, "/v1/payments", tc.body, devSecret, withIdem("v-"+strconv.Itoa(i)))
		assert.Equal(t, tc.status, resp.StatusCode, tc.body)
		_, code = errCode(t, body)
		assert.Equal(t, tc.code, code, tc.body)
	}

	// 未知路徑 404。
	resp, _ = env.do(t, http.MethodGet, "/v1/nope", "", devSecret)
	assert.Equal(t, 404, resp.StatusCode)
}

func TestRateLimit(t *testing.T) {
	env := newEnv(t, &DevVerifier{APIKey: devKey, SigningSecret: devSecret, MerchantID: devMch}, 3)
	var last *http.Response
	var body []byte
	for i := range 5 {
		last, body = env.do(t, http.MethodGet, "/v1/payments/pay_"+strconv.Itoa(i), "", devSecret)
	}
	assert.Equal(t, 429, last.StatusCode)
	assert.Equal(t, "1", last.Header.Get("Retry-After"))
	typ, code := errCode(t, body)
	assert.Equal(t, "rate_limit_error", typ)
	assert.Equal(t, "rate_limit_exceeded", code)
}

func TestProviderWebhook(t *testing.T) {
	env := newEnv(t, &DevVerifier{APIKey: devKey, SigningSecret: devSecret, MerchantID: devMch})
	post := func(provider string) (int, string) {
		resp, err := http.Post(env.srv.URL+"/psp/"+provider+"/webhook", "application/json", strings.NewReader(`{"type":"capture.succeeded"}`))
		require.NoError(t, err)
		defer resp.Body.Close()
		b := must(io.ReadAll(resp.Body))
		return resp.StatusCode, string(b)
	}
	code, body := post("mock")
	assert.Equal(t, 200, code)
	assert.Contains(t, body, `"received":true`)
	code, body = post("bad")
	assert.Equal(t, 400, code)
	assert.Contains(t, body, "webhook_signature_invalid")
	code, _ = post("unknown")
	assert.Equal(t, 404, code)
}

func TestGRPCVerifierCache(t *testing.T) {
	calls := 0
	fm := &fakeMerchants{fn: func(req *merchantv1.VerifyApiKeyRequest) (*merchantv1.VerifyApiKeyResponse, error) {
		calls++
		switch req.GetKey() {
		case "pk_test_ok":
			return &merchantv1.VerifyApiKeyResponse{Valid: true, MerchantId: "m1", ApiKeyId: "k1", Mode: merchantv1.ApiKeyMode_API_KEY_MODE_TEST, SigningSecret: "s", PreviousSigningSecret: "p", MerchantStatus: merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE}, nil
		case "pk_test_revoked":
			return &merchantv1.VerifyApiKeyResponse{Valid: false, Reason: "revoked"}, nil
		case "pk_test_expired":
			return &merchantv1.VerifyApiKeyResponse{Valid: false, Reason: "expired"}, nil
		default:
			return &merchantv1.VerifyApiKeyResponse{Valid: false, Reason: "not_found"}, nil
		}
	}}
	v := NewGRPCVerifier(fm)
	p, err := v.VerifyKey(context.Background(), "pk_test_ok")
	require.NoError(t, err)
	assert.Equal(t, "m1", p.MerchantID)
	assert.Equal(t, "p", p.PreviousSigningSecret)
	assert.False(t, p.LiveMode)
	_ = must(v.VerifyKey(context.Background(), "pk_test_ok"))
	assert.Equal(t, 1, calls, "cached")
	_, err = v.VerifyKey(context.Background(), "pk_test_revoked")
	require.ErrorIs(t, err, errAPIKeyRevoked)
	_, err = v.VerifyKey(context.Background(), "pk_test_expired")
	require.ErrorIs(t, err, errAPIKeyExpired)
	_, err = v.VerifyKey(context.Background(), "pk_test_nope")
	require.ErrorIs(t, err, errInvalidAPIKey)
	assert.True(t, p.HasScope("anything"))
	p.Scopes = []string{"payments:read"}
	assert.False(t, p.HasScope("payments:write"))
}

type fakeMerchants struct {
	merchantv1.MerchantServiceClient
	fn func(*merchantv1.VerifyApiKeyRequest) (*merchantv1.VerifyApiKeyResponse, error)
}

//nolint:revive // 方法名由 proto 產生
func (f *fakeMerchants) VerifyApiKey(_ context.Context, req *merchantv1.VerifyApiKeyRequest, _ ...grpc.CallOption) (*merchantv1.VerifyApiKeyResponse, error) {
	return f.fn(req)
}

func TestRequestHashCanonical(t *testing.T) {
	a := requestHash("POST", "/v1/payments", []byte(`{"b":1,"a":{"y":2,"x":1}}`))
	b := requestHash("POST", "/v1/payments", []byte(` { "a" : { "x":1, "y":2 }, "b":1 } `))
	assert.Equal(t, a, b)
	assert.NotEqual(t, a, requestHash("POST", "/v1/refunds", []byte(`{"b":1,"a":{"y":2,"x":1}}`)))
	assert.Equal(t, requestHash("POST", "/x", nil), requestHash("POST", "/x", []byte("{}")))
	assert.True(t, validIdempotencyKey("abc-123"))
	assert.False(t, validIdempotencyKey(strings.Repeat("a", 256)))
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	require.True(t, ok, "expected object, got %T", v)
	return m
}

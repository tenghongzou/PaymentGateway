package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/money"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

var testNow = time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

type harness struct {
	svc    *Service
	repo   *fakeRepo
	outbox *fakeOutbox
	router *fakeRouter
	reg    fakeRegistry
	clock  *fakeClock
}

func newHarness(t *testing.T, reg fakeRegistry, candidates []Candidate, cfg Config) *harness {
	t.Helper()
	h := &harness{repo: newFakeRepo(), outbox: &fakeOutbox{}, router: &fakeRouter{candidates: candidates}, reg: reg, clock: &fakeClock{t: testNow}}
	if len(cfg.ResolveDelays) == 0 {
		cfg.ResolveDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	}
	h.svc = NewService(Deps{Repo: h.repo, Tx: fakeTx{}, Outbox: h.outbox, Providers: reg, Router: h.router, Clock: h.clock, Config: cfg})
	return h
}

func cardCmd(token, idem string, cm domain.CaptureMethod) CreatePaymentCommand {
	return CreatePaymentCommand{
		MerchantID: "m-1", IdempotencyKey: idem, RequestHash: "h-" + idem, Amount: money.MustNew(1000, "TWD"), CaptureMethod: cm,
		PaymentMethodType: "card", MethodDetails: domain.PaymentMethodDetails{TokenProvider: "mock"},
		Instrument: &providerv1.PaymentInstrument{Instrument: &providerv1.PaymentInstrument_CardToken{CardToken: &providerv1.CardToken{Token: token, TokenProvider: "mock"}}},
		Customer:   domain.Customer{ID: "cus_1"}, Metadata: map[string]string{"order": "1"},
	}
}

func TestCreatePaymentAutomaticSuccess(t *testing.T) {
	reg := fakeRegistry{"mock": &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(true)}}}
	h := newHarness(t, reg, []Candidate{{Provider: "mock", Reason: "default"}}, Config{})

	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.False(t, res.Replayed)
	p := res.Payment
	assert.Equal(t, domain.StatusCaptured, p.Status)
	assert.Equal(t, money.MustNew(1000, "TWD"), p.AmountCaptured)
	assert.Equal(t, "mock", p.SelectedProvider)
	assert.Equal(t, "visa", p.PaymentMethodDetails.Brand)
	assert.Equal(t, 3, p.Version)
	require.Len(t, p.Attempts, 1)
	assert.Equal(t, domain.AttemptApproved, p.Attempts[0].Status)
	assert.Equal(t, p.Attempts[0].PublicID(), reg["mock"].calls[0].idemKey, "att_id is the PSP idempotency key")

	// 事件 + outbox：created / authorized / captured。
	assert.Equal(t, []string{domain.EventPaymentCreated, domain.EventPaymentAuthorized, domain.EventPaymentCaptured}, h.repo.eventTypes())
	require.Len(t, h.outbox.msgs, 3)
	assert.Equal(t, p.PublicID, h.outbox.msgs[2].AggregateID)
	var ev paymentv1.PaymentEvent
	require.NoError(t, proto.Unmarshal(h.outbox.msgs[2].Payload, &ev))
	assert.Equal(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, ev.GetEventType())
	assert.Equal(t, int64(3), ev.GetPaymentVersion())
	assert.Equal(t, int64(1000), ev.GetPaymentCaptured().GetTotalCapturedAmount().GetAmountMinor())
	assert.Equal(t, int64(59), ev.GetPaymentCaptured().GetFee().GetAmountMinor())
	assert.Equal(t, []string{"mock:"}, h.router.reports)

	// 持久化後可讀回。
	got, err := h.svc.GetPayment(context.Background(), "m-1", p.PublicID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCaptured, got.Status)
	_, err = h.svc.GetPayment(context.Background(), "other", p.PublicID)
	require.ErrorIs(t, err, domain.ErrPaymentNotFound)
}

func TestCreatePaymentIdempotentReplayAndMismatch(t *testing.T) {
	reg := fakeRegistry{"mock": &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(true)}}}
	h := newHarness(t, reg, []Candidate{{Provider: "mock"}}, Config{})
	first, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)

	again, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Payment.PublicID, again.Payment.PublicID)
	assert.Len(t, reg["mock"].calls, 1, "provider not called again")

	cmd := cardCmd("tok_ok", "k1", domain.CaptureAutomatic)
	cmd.RequestHash = "different"
	_, err = h.svc.CreatePayment(context.Background(), cmd)
	require.ErrorIs(t, err, apperr.ErrIdempotencyMismatch)

	// 唯一索引衝突路徑（GetPaymentByIdempotencyKey 未命中但 INSERT 衝突）：直接模擬。
	h.repo.mu.Lock()
	p := h.repo.payments[first.Payment.PublicID]
	p.IdempotencyKey = "k-race"
	h.repo.mu.Unlock()
	h.repo.payments[first.Payment.PublicID].IdempotencyRequestHash = "h-k-race"
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k-race", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.True(t, res.Replayed)
}

func TestCreatePaymentFailoverSuccess(t *testing.T) {
	reg := fakeRegistry{
		"mock":   &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){transportErr(codes.Unavailable)}},
		"stripe": &fakeProvider{name: "stripe", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(false)}},
	}
	h := newHarness(t, reg, []Candidate{{Provider: "mock", Reason: "default"}, {Provider: "stripe", Reason: "default"}}, Config{MaxAttempts: 2})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_unavailable", "k1", domain.CaptureManual))
	require.NoError(t, err)
	p := res.Payment
	assert.Equal(t, domain.StatusAuthorized, p.Status)
	assert.Equal(t, "stripe", p.SelectedProvider)
	require.Len(t, p.Attempts, 2)
	assert.Equal(t, domain.AttemptUnavailable, p.Attempts[0].Status)
	assert.Equal(t, domain.CategoryProviderUnavailable, p.Attempts[0].ErrorCategory)
	assert.Equal(t, domain.AttemptApproved, p.Attempts[1].Status)
	assert.Equal(t, "fallback", p.Attempts[1].RouteReason)
	assert.NotEqual(t, reg["mock"].calls[0].idemKey, reg["stripe"].calls[0].idemKey, "each attempt uses its own PSP idempotency key")
	assert.Equal(t, []string{"mock:provider_unavailable", "stripe:"}, h.router.reports)
	assert.Equal(t, []string{domain.EventPaymentCreated, domain.EventPaymentAuthorized}, h.repo.eventTypes())
	assert.Equal(t, money.MustNew(1000, "TWD"), p.AmountAuthorized)
	assert.True(t, p.AmountCaptured.IsZero())
}

func TestCreatePaymentHardDeclineNoFailover(t *testing.T) {
	reg := fakeRegistry{
		"mock":   &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){declined(providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD, "insufficient_funds")}},
		"stripe": &fakeProvider{name: "stripe", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(true)}},
	}
	h := newHarness(t, reg, []Candidate{{Provider: "mock"}, {Provider: "stripe"}}, Config{MaxAttempts: 3})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_decline_hard", "k1", domain.CaptureAutomatic))
	require.NoError(t, err, "business decline is not an error")
	p := res.Payment
	assert.Equal(t, domain.StatusFailed, p.Status)
	require.NotNil(t, p.Failure)
	assert.Equal(t, domain.CategoryDeclinedHard, p.Failure.Category)
	assert.Equal(t, "insufficient_funds", p.Failure.Code)
	assert.True(t, p.Failure.Retryable)
	require.Len(t, p.Attempts, 1)
	assert.Empty(t, reg["stripe"].calls, "hard decline must not failover")
	assert.Equal(t, []string{domain.EventPaymentCreated, domain.EventPaymentFailed}, h.repo.eventTypes())
	var ev paymentv1.PaymentEvent
	require.NoError(t, proto.Unmarshal(h.outbox.msgs[1].Payload, &ev))
	assert.Equal(t, "insufficient_funds", ev.GetPaymentFailed().GetErrorCode())
	assert.Equal(t, int32(1), ev.GetPaymentFailed().GetAttemptCount())
}

func TestCreatePaymentFraudHidesDeclineCode(t *testing.T) {
	reg := fakeRegistry{"mock": &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){declined(providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_FRAUD_SUSPECTED, "stolen_card")}}}
	h := newHarness(t, reg, []Candidate{{Provider: "mock"}}, Config{})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_fraud", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, res.Payment.Status)
	assert.Equal(t, "generic_decline", res.Payment.Failure.PublicCode())
	assert.False(t, res.Payment.Failure.Retryable)
}

func TestCreatePaymentSoftDeclineWhitelistFailover(t *testing.T) {
	reg := fakeRegistry{
		"mock":   &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){declined(providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_SOFT, "try_again_later")}},
		"stripe": &fakeProvider{name: "stripe", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(true)}},
	}
	h := newHarness(t, reg, []Candidate{{Provider: "mock"}, {Provider: "stripe"}}, Config{MaxAttempts: 2})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_decline_soft", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCaptured, res.Payment.Status)
	assert.Len(t, res.Payment.Attempts, 2)
}

func TestCreatePaymentUnavailableOnceRetrySameProvider(t *testing.T) {
	reg := fakeRegistry{"mock": &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){transportErr(codes.Unavailable), approved(true)}}}
	h := newHarness(t, reg, []Candidate{{Provider: "mock"}}, Config{MaxAttempts: 2, RetrySameProviderOnUnavailable: true})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_unavailable_once", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCaptured, res.Payment.Status)
	require.Len(t, res.Payment.Attempts, 2)
	assert.Equal(t, "retry", res.Payment.Attempts[1].RouteReason)

	// 關閉同 Provider 重試 → failed。
	reg2 := fakeRegistry{"mock": &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){transportErr(codes.Unavailable), approved(true)}}}
	h2 := newHarness(t, reg2, []Candidate{{Provider: "mock"}}, Config{MaxAttempts: 2, RetrySameProviderOnUnavailable: false})
	res2, err := h2.svc.CreatePayment(context.Background(), cardCmd("tok_unavailable_once", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, res2.Payment.Status)
	assert.Equal(t, domain.CategoryProviderUnavailable, res2.Payment.Failure.Category)
}

func TestCreatePaymentMaxAttemptsRespected(t *testing.T) {
	reg := fakeRegistry{
		"a": &fakeProvider{name: "a", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){transportErr(codes.Unavailable)}},
		"b": &fakeProvider{name: "b", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){transportErr(codes.Unavailable)}},
		"c": &fakeProvider{name: "c", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(true)}},
	}
	h := newHarness(t, reg, []Candidate{{Provider: "a"}, {Provider: "b"}, {Provider: "c"}}, Config{MaxAttempts: 2})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, res.Payment.Status)
	assert.Len(t, res.Payment.Attempts, 2)
	assert.Empty(t, reg["c"].calls)
}

func TestCreatePaymentTimeoutResolvesViaStatus(t *testing.T) {
	fp := &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){transportErr(codes.DeadlineExceeded)}}
	fp.status = func(*providerv1.GetPaymentStatusRequest) (*providerv1.GetPaymentStatusResponse, error) {
		return &providerv1.GetPaymentStatusResponse{Result: &providerv1.ProviderResult{Success: true, ProviderReference: "ref_recovered"}, Status: providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_CAPTURED}, nil
	}
	h := newHarness(t, fakeRegistry{"mock": fp}, []Candidate{{Provider: "mock"}}, Config{})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_timeout", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCaptured, res.Payment.Status)
	assert.Equal(t, "ref_recovered", res.Payment.ProviderReference)

	// 查無紀錄 → unavailable → 可 failover（此處無其他候選、不重試 → failed）。
	fp2 := &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){transportErr(codes.DeadlineExceeded)}}
	h2 := newHarness(t, fakeRegistry{"mock": fp2}, []Candidate{{Provider: "mock"}}, Config{RetrySameProviderOnUnavailable: false})
	res2, err := h2.svc.CreatePayment(context.Background(), cardCmd("tok_timeout", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, res2.Payment.Status)
	assert.Equal(t, domain.AttemptUnavailable, res2.Payment.Attempts[0].Status)

	// 仍不明 → payment 維持 created、attempt unknown。
	fp3 := &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){transportErr(codes.DeadlineExceeded)}}
	fp3.status = func(*providerv1.GetPaymentStatusRequest) (*providerv1.GetPaymentStatusResponse, error) {
		return nil, errBoom
	}
	h3 := newHarness(t, fakeRegistry{"mock": fp3}, []Candidate{{Provider: "mock"}}, Config{})
	res3, err := h3.svc.CreatePayment(context.Background(), cardCmd("tok_timeout", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCreated, res3.Payment.Status)
	assert.Equal(t, domain.AttemptUnknown, res3.Payment.Attempts[0].Status)
}

func TestCreatePaymentRequiresActionThenConfirm(t *testing.T) {
	fp := &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){requiresAction()}}
	fp.status = func(*providerv1.GetPaymentStatusRequest) (*providerv1.GetPaymentStatusResponse, error) {
		return &providerv1.GetPaymentStatusResponse{Result: &providerv1.ProviderResult{Success: true, ProviderReference: "ref_3ds"}, Status: providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_AUTHORIZED}, nil
	}
	h := newHarness(t, fakeRegistry{"mock": fp}, []Candidate{{Provider: "mock"}}, Config{})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_3ds", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	p := res.Payment
	assert.Equal(t, domain.StatusRequiresAction, p.Status)
	require.NotNil(t, p.Attempts[0].NextAction)
	assert.Equal(t, "http://mock/3ds/x", p.Attempts[0].NextAction.URL)
	assert.Equal(t, testNow.Add(30*time.Minute), *p.ExpiresAt)

	confirmed, err := h.svc.ConfirmPayment(context.Background(), ConfirmCommand{MerchantID: "m-1", PaymentID: p.PublicID, IdempotencyKey: "c1"})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCaptured, confirmed.Status, "automatic: authorized then captured via provider Capture")
	assert.Equal(t, domain.AttemptApproved, confirmed.Attempts[0].Status)
	assert.Equal(t, []string{domain.EventPaymentCreated, domain.EventPaymentRequiresAction, domain.EventPaymentAuthorized, domain.EventPaymentCaptured}, h.repo.eventTypes())

	// 重複確認回目前狀態。
	again, err := h.svc.ConfirmPayment(context.Background(), ConfirmCommand{MerchantID: "m-1", PaymentID: p.PublicID})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCaptured, again.Status)

	// 3DS 失敗 → failed。
	fp2 := &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){requiresAction()}}
	fp2.status = func(*providerv1.GetPaymentStatusRequest) (*providerv1.GetPaymentStatusResponse, error) {
		return &providerv1.GetPaymentStatusResponse{Result: &providerv1.ProviderResult{Success: true}, Status: providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_FAILED}, nil
	}
	h2 := newHarness(t, fakeRegistry{"mock": fp2}, []Candidate{{Provider: "mock"}}, Config{})
	res2 := must(h2.svc.CreatePayment(context.Background(), cardCmd("tok_3ds", "k1", domain.CaptureManual)))
	failed, err := h2.svc.ConfirmPayment(context.Background(), ConfirmCommand{MerchantID: "m-1", PaymentID: res2.Payment.PublicID})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, failed.Status)
	assert.Equal(t, domain.CategoryAuthenticationFailed, failed.Failure.Category)
}

func TestCreatePaymentValidationAndRouting(t *testing.T) {
	reg := fakeRegistry{"mock": &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(true)}}}
	h := newHarness(t, reg, nil, Config{})
	_, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k1", domain.CaptureAutomatic))
	require.ErrorIs(t, err, domain.ErrNoRouteAvailable)
	assert.Empty(t, h.repo.payments, "no payment created when no route")

	h.router.candidates = []Candidate{{Provider: "mock"}}
	cmd := cardCmd("tok_ok", "k2", domain.CaptureAutomatic)
	cmd.Amount = money.Zero("TWD")
	_, err = h.svc.CreatePayment(context.Background(), cmd)
	require.ErrorIs(t, err, domain.ErrAmountTooSmall)

	cmd = cardCmd("tok_ok", "k3", domain.CaptureAutomatic)
	cmd.Instrument = nil
	_, err = h.svc.CreatePayment(context.Background(), cmd)
	require.ErrorIs(t, err, domain.ErrPaymentMethodMissing)

	cmd = cardCmd("tok_ok", "", domain.CaptureAutomatic)
	_, err = h.svc.CreatePayment(context.Background(), cmd)
	require.Error(t, err)

	// provider 未設定 → unavailable → failed。
	h.router.candidates = []Candidate{{Provider: "ghost"}}
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k4", domain.CaptureAutomatic))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, res.Payment.Status)
}

func TestCaptureVoidFlow(t *testing.T) {
	fp := &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(false)}}
	h := newHarness(t, fakeRegistry{"mock": fp}, []Candidate{{Provider: "mock"}}, Config{})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k1", domain.CaptureManual))
	require.NoError(t, err)
	id := res.Payment.PublicID
	assert.Equal(t, domain.StatusAuthorized, res.Payment.Status)

	// 超額。
	over := money.MustNew(1001, "TWD")
	_, err = h.svc.CapturePayment(context.Background(), CaptureCommand{MerchantID: "m-1", PaymentID: id, IdempotencyKey: "c1", Amount: &over})
	require.ErrorIs(t, err, domain.ErrAmountExceedsAuthorized)
	// 幣別不符。
	usd := money.MustNew(10, "USD")
	_, err = h.svc.CapturePayment(context.Background(), CaptureCommand{MerchantID: "m-1", PaymentID: id, Amount: &usd})
	require.ErrorIs(t, err, domain.ErrCurrencyMismatch)
	// PSP 拒絕 → provider_error，狀態不變。
	fp.capture = func(req *providerv1.CaptureRequest) (*providerv1.CaptureResponse, error) {
		return &providerv1.CaptureResponse{Result: &providerv1.ProviderResult{Success: false, ErrorCategory: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD, ProviderErrorCode: "do_not_honor", ProviderErrorMessage: "nope"}}, nil
	}
	_, err = h.svc.CapturePayment(context.Background(), CaptureCommand{MerchantID: "m-1", PaymentID: id, IdempotencyKey: "c1"})
	require.Error(t, err)
	assert.Equal(t, "card_declined", apperr.From(err).Code)
	cur := must(h.svc.GetPayment(context.Background(), "m-1", id))
	assert.Equal(t, domain.StatusAuthorized, cur.Status, "capture failure keeps authorized")
	// PSP 逾時 → 504。
	fp.capture = func(*providerv1.CaptureRequest) (*providerv1.CaptureResponse, error) {
		return nil, context.DeadlineExceeded
	}
	_, err = h.svc.CapturePayment(context.Background(), CaptureCommand{MerchantID: "m-1", PaymentID: id})
	require.ErrorIs(t, err, domain.ErrProviderTimeout)

	// 部分 capture 成功。
	fp.capture = nil
	part := money.MustNew(600, "TWD")
	p, err := h.svc.CapturePayment(context.Background(), CaptureCommand{MerchantID: "m-1", PaymentID: id, IdempotencyKey: "c1", Amount: &part, Final: true})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCaptured, p.Status)
	assert.Equal(t, part, p.AmountCaptured)
	// 重複 capture（冪等）回目前狀態。
	p2, err := h.svc.CapturePayment(context.Background(), CaptureCommand{MerchantID: "m-1", PaymentID: id, IdempotencyKey: "c1"})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCaptured, p2.Status)
	// 已 capture 不可 void。
	_, err = h.svc.VoidPayment(context.Background(), VoidCommand{MerchantID: "m-1", PaymentID: id})
	require.ErrorIs(t, err, domain.ErrVoidNotAllowed)

	// 樂觀鎖衝突：tx2 前版本被改。
	res2 := must(h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k2", domain.CaptureManual)))
	fp.capture = func(req *providerv1.CaptureRequest) (*providerv1.CaptureResponse, error) {
		h.repo.mu.Lock()
		h.repo.payments[res2.Payment.PublicID].Version++
		h.repo.mu.Unlock()
		return &providerv1.CaptureResponse{Result: &providerv1.ProviderResult{Success: true}, CapturedAmount: req.GetAmount()}, nil
	}
	_, err = h.svc.CapturePayment(context.Background(), CaptureCommand{MerchantID: "m-1", PaymentID: res2.Payment.PublicID})
	require.ErrorIs(t, err, pgdb.ErrConcurrentModification)

	// void 成功。
	fp.capture = nil
	res3 := must(h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k3", domain.CaptureManual)))
	v, err := h.svc.VoidPayment(context.Background(), VoidCommand{MerchantID: "m-1", PaymentID: res3.Payment.PublicID, Reason: "requested_by_customer"})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusVoided, v.Status)
	assert.Equal(t, domain.VoidReasonMerchantRequest, *v.VoidReason)
	v2, err := h.svc.VoidPayment(context.Background(), VoidCommand{MerchantID: "m-1", PaymentID: res3.Payment.PublicID})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusVoided, v2.Status, "void is idempotent")
}

func TestRefundFlow(t *testing.T) {
	fp := &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(true)}}
	h := newHarness(t, fakeRegistry{"mock": fp}, []Candidate{{Provider: "mock"}}, Config{})
	res, err := h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k1", domain.CaptureAutomatic))
	require.NoError(t, err)
	id := res.Payment.PublicID

	amt := money.MustNew(600, "TWD")
	r1, err := h.svc.CreateRefund(context.Background(), CreateRefundCommand{MerchantID: "m-1", PaymentID: id, IdempotencyKey: "r1", Amount: &amt, Reason: "requested_by_customer"})
	require.NoError(t, err)
	assert.False(t, r1.Replayed)
	assert.Equal(t, domain.RefundSucceeded, r1.Refund.Status)
	assert.Equal(t, "mock_re_1", r1.Refund.ProviderReference)
	p := must(h.svc.GetPayment(context.Background(), "m-1", id))
	assert.Equal(t, domain.StatusPartiallyRefunded, p.Status)
	assert.Equal(t, amt, p.AmountRefunded)
	assert.True(t, p.AmountRefundPending.IsZero())

	// 冪等重放。
	again, err := h.svc.CreateRefund(context.Background(), CreateRefundCommand{MerchantID: "m-1", PaymentID: id, IdempotencyKey: "r1", Amount: &amt})
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, r1.Refund.PublicID, again.Refund.PublicID)

	// 超額。
	over := money.MustNew(500, "TWD")
	_, err = h.svc.CreateRefund(context.Background(), CreateRefundCommand{MerchantID: "m-1", PaymentID: id, IdempotencyKey: "r2", Amount: &over})
	require.ErrorIs(t, err, domain.ErrRefundExceedsAvailable)

	// PSP 拒絕 → refund failed、釋放保留額。
	fp.refund = func(*providerv1.RefundRequest) (*providerv1.RefundResponse, error) {
		return &providerv1.RefundResponse{Status: providerv1.RefundState_REFUND_STATE_FAILED, Result: &providerv1.ProviderResult{Success: false, ErrorCategory: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD, ProviderErrorCode: "refund_declined"}}, nil
	}
	rest := money.MustNew(400, "TWD")
	r3, err := h.svc.CreateRefund(context.Background(), CreateRefundCommand{MerchantID: "m-1", PaymentID: id, IdempotencyKey: "r3", Amount: &rest})
	require.NoError(t, err)
	assert.Equal(t, domain.RefundFailed, r3.Refund.Status)
	assert.Equal(t, "card_declined", r3.Refund.FailureCode)
	p = must(h.svc.GetPayment(context.Background(), "m-1", id))
	assert.Equal(t, rest, p.AvailableToRefund())

	// 逾時 → pending。
	fp.refund = func(*providerv1.RefundRequest) (*providerv1.RefundResponse, error) {
		return nil, context.DeadlineExceeded
	}
	r4, err := h.svc.CreateRefund(context.Background(), CreateRefundCommand{MerchantID: "m-1", PaymentID: id, IdempotencyKey: "r4", Amount: &rest})
	require.NoError(t, err)
	assert.Equal(t, domain.RefundPending, r4.Refund.Status)
	p = must(h.svc.GetPayment(context.Background(), "m-1", id))
	assert.True(t, p.AvailableToRefund().IsZero(), "pending refund reserves the amount")
	got, err := h.svc.GetRefund(context.Background(), "m-1", r4.Refund.PublicID)
	require.NoError(t, err)
	assert.Equal(t, domain.RefundPending, got.Status)
	_, err = h.svc.GetRefund(context.Background(), "m-2", r4.Refund.PublicID)
	require.ErrorIs(t, err, domain.ErrRefundNotFound)

	// 全額退款（預設金額 = 可退）— 先建立新付款。
	fp.refund = nil
	res2 := must(h.svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k2", domain.CaptureAutomatic)))
	r5, err := h.svc.CreateRefund(context.Background(), CreateRefundCommand{MerchantID: "m-1", PaymentID: res2.Payment.PublicID, IdempotencyKey: "r5"})
	require.NoError(t, err)
	assert.Equal(t, money.MustNew(1000, "TWD"), r5.Refund.Amount)
	p2 := must(h.svc.GetPayment(context.Background(), "m-1", res2.Payment.PublicID))
	assert.Equal(t, domain.StatusRefunded, p2.Status)

	// 未 capture 不可退款。
	res3 := must(newHarness(t, fakeRegistry{"mock": &fakeProvider{name: "mock", authorize: []func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error){approved(false)}}}, []Candidate{{Provider: "mock"}}, Config{}).svc.CreatePayment(context.Background(), cardCmd("tok_ok", "k9", domain.CaptureManual)))
	_ = res3
	_, err = h.svc.CreateRefund(context.Background(), CreateRefundCommand{MerchantID: "m-1", PaymentID: "pay_nope", IdempotencyKey: "r6"})
	require.ErrorIs(t, err, domain.ErrPaymentNotFound)
}

func TestListPaymentsClampsLimit(t *testing.T) {
	h := newHarness(t, fakeRegistry{}, nil, Config{})
	_, _, err := h.svc.ListPayments(context.Background(), "m-1", ListFilter{Limit: 500})
	require.NoError(t, err)
}

func TestEvaluateAuthorizeEdgeCases(t *testing.T) {
	out := evaluateAuthorize(&providerv1.AuthorizeResponse{}, nil, "TWD")
	assert.Equal(t, outcomeFailed, out.kind)
	assert.Equal(t, domain.CategoryUnknown, out.category)

	out = evaluateAuthorize(&providerv1.AuthorizeResponse{Result: &providerv1.ProviderResult{Success: true}, Status: providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED}, nil, "TWD")
	assert.Equal(t, outcomeFailed, out.kind)

	out = evaluateAuthorize(nil, errBoom, "TWD")
	assert.Equal(t, domain.CategoryUnknown, out.category)

	cat, transportCode := classifyTransportError(context.DeadlineExceeded)
	assert.Equal(t, "deadline_exceeded", transportCode)
	assert.Equal(t, domain.CategoryProviderTimeout, cat)
	assert.Equal(t, domain.CategoryProviderRateLimited, categoryFromProto(providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_PROVIDER_UNAVAILABLE, "rate_limited"))
	assert.Equal(t, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_PROVIDER_UNAVAILABLE, CategoryToProto(domain.CategoryProviderRateLimited))
	assert.Equal(t, domain.StatusCaptured, StatusFromProto(StatusToProto(domain.StatusCaptured)))
}

package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// ---- fake repo（記憶體，模擬唯一索引與樂觀鎖）----

type fakeRepo struct {
	mu       sync.Mutex
	payments map[string]*domain.Payment // public id → payment
	refunds  map[string]*domain.Refund
	events   []domain.Event
	attempts map[string]*domain.Attempt
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{payments: map[string]*domain.Payment{}, refunds: map[string]*domain.Refund{}, attempts: map[string]*domain.Attempt{}}
}

func clonePayment(p *domain.Payment) *domain.Payment {
	c := *p
	c.Attempts = make([]*domain.Attempt, len(p.Attempts))
	for i, a := range p.Attempts {
		ac := *a
		c.Attempts[i] = &ac
	}
	return &c
}

func (r *fakeRepo) CreatePayment(_ context.Context, p *domain.Payment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.payments {
		if e.MerchantID == p.MerchantID && e.IdempotencyKey == p.IdempotencyKey {
			return ErrDuplicateIdempotencyKey
		}
	}
	c := clonePayment(p)
	c.Attempts = nil
	r.payments[p.PublicID] = c
	return nil
}

func (r *fakeRepo) get(merchantID, id string) (*domain.Payment, error) {
	p, ok := r.payments[id]
	if !ok || p.MerchantID != merchantID {
		return nil, domain.ErrPaymentNotFound
	}
	c := clonePayment(p)
	c.Attempts = nil
	for _, a := range r.attempts {
		if a.PaymentID == p.ID {
			ac := *a
			c.Attempts = append(c.Attempts, &ac)
		}
	}
	// 依 attempt_no 排序
	for i := range c.Attempts {
		for j := i + 1; j < len(c.Attempts); j++ {
			if c.Attempts[j].AttemptNo < c.Attempts[i].AttemptNo {
				c.Attempts[i], c.Attempts[j] = c.Attempts[j], c.Attempts[i]
			}
		}
	}
	return c, nil
}

func (r *fakeRepo) GetPayment(_ context.Context, merchantID, id string) (*domain.Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.get(merchantID, id)
}

func (r *fakeRepo) GetPaymentByIdempotencyKey(_ context.Context, merchantID, key string) (*domain.Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.payments {
		if p.MerchantID == merchantID && p.IdempotencyKey == key {
			return r.get(merchantID, p.PublicID)
		}
	}
	return nil, domain.ErrPaymentNotFound
}

func (r *fakeRepo) GetPaymentForUpdate(ctx context.Context, merchantID, id string) (*domain.Payment, error) {
	return r.GetPayment(ctx, merchantID, id)
}

func (r *fakeRepo) UpdatePayment(_ context.Context, p *domain.Payment, expected int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.payments[p.PublicID]
	if !ok {
		return domain.ErrPaymentNotFound
	}
	if cur.Version != expected {
		return pgdb.ErrConcurrentModification
	}
	c := clonePayment(p)
	c.Attempts = nil
	r.payments[p.PublicID] = c
	return nil
}

func (r *fakeRepo) InsertAttempt(_ context.Context, a *domain.Attempt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac := *a
	r.attempts[a.ID] = &ac
	return nil
}

func (r *fakeRepo) UpdateAttempt(ctx context.Context, a *domain.Attempt) error {
	return r.InsertAttempt(ctx, a)
}

func (r *fakeRepo) AppendEvents(_ context.Context, _ *domain.Payment, events []domain.Event, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, events...)
	return nil
}

func (r *fakeRepo) ListPayments(_ context.Context, merchantID string, _ ListFilter) ([]*domain.Payment, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.Payment
	for id, p := range r.payments {
		if p.MerchantID == merchantID {
			if c, err := r.get(merchantID, id); err == nil {
				out = append(out, c)
			}
		}
	}
	return out, "", nil
}

func (r *fakeRepo) CreateRefund(_ context.Context, rf *domain.Refund) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.refunds {
		if e.MerchantID == rf.MerchantID && e.IdempotencyKey == rf.IdempotencyKey {
			return ErrDuplicateIdempotencyKey
		}
	}
	c := *rf
	r.refunds[rf.PublicID] = &c
	return nil
}

func (r *fakeRepo) GetRefund(_ context.Context, merchantID, id string) (*domain.Refund, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rf, ok := r.refunds[id]
	if !ok || rf.MerchantID != merchantID {
		return nil, domain.ErrRefundNotFound
	}
	c := *rf
	return &c, nil
}

func (r *fakeRepo) GetRefundByIdempotencyKey(_ context.Context, merchantID, key string) (*domain.Refund, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rf := range r.refunds {
		if rf.MerchantID == merchantID && rf.IdempotencyKey == key {
			c := *rf
			return &c, nil
		}
	}
	return nil, domain.ErrRefundNotFound
}

func (r *fakeRepo) UpdateRefund(_ context.Context, rf *domain.Refund, expected int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.refunds[rf.PublicID]
	if !ok {
		return domain.ErrRefundNotFound
	}
	if cur.Version != expected {
		return pgdb.ErrConcurrentModification
	}
	c := *rf
	r.refunds[rf.PublicID] = &c
	return nil
}

func (r *fakeRepo) eventTypes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

// ---- fake tx / outbox / clock / router ----

type fakeTx struct{}

func (fakeTx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

type fakeOutbox struct {
	mu   sync.Mutex
	msgs []outbox.Message
}

func (o *fakeOutbox) Insert(_ context.Context, m outbox.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.msgs = append(o.msgs, m)
	return nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

type fakeRouter struct {
	candidates []Candidate
	reports    []string
	err        error
}

func (r *fakeRouter) Route(context.Context, RoutingContext) ([]Candidate, error) {
	return r.candidates, r.err
}

func (r *fakeRouter) Report(provider string, cat domain.ProviderErrorCategory) {
	r.reports = append(r.reports, provider+":"+string(cat))
}

// ---- fake provider ----

type authCall struct {
	provider string
	idemKey  string
}

type fakeProvider struct {
	name string
	// authorize 依呼叫次數回傳（最後一個重複使用）。
	authorize []func(req *providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error)
	capture   func(req *providerv1.CaptureRequest) (*providerv1.CaptureResponse, error)
	void      func(req *providerv1.VoidRequest) (*providerv1.VoidResponse, error)
	refund    func(req *providerv1.RefundRequest) (*providerv1.RefundResponse, error)
	status    func(req *providerv1.GetPaymentStatusRequest) (*providerv1.GetPaymentStatusResponse, error)
	calls     []authCall
	n         int
}

func (f *fakeProvider) Authorize(_ context.Context, req *providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
	f.calls = append(f.calls, authCall{f.name, req.GetIdempotencyKey()})
	i := min(f.n, len(f.authorize)-1)
	f.n++
	return f.authorize[i](req)
}

func (f *fakeProvider) Capture(_ context.Context, req *providerv1.CaptureRequest) (*providerv1.CaptureResponse, error) {
	if f.capture == nil {
		return &providerv1.CaptureResponse{Result: &providerv1.ProviderResult{Success: true, ProviderReference: req.GetProviderReference()}, CapturedAmount: req.GetAmount()}, nil
	}
	return f.capture(req)
}

func (f *fakeProvider) Void(_ context.Context, req *providerv1.VoidRequest) (*providerv1.VoidResponse, error) {
	if f.void == nil {
		return &providerv1.VoidResponse{Result: &providerv1.ProviderResult{Success: true, ProviderReference: req.GetProviderReference()}}, nil
	}
	return f.void(req)
}

func (f *fakeProvider) Refund(_ context.Context, req *providerv1.RefundRequest) (*providerv1.RefundResponse, error) {
	if f.refund == nil {
		return &providerv1.RefundResponse{Result: &providerv1.ProviderResult{Success: true, ProviderReference: req.GetProviderReference()}, Status: providerv1.RefundState_REFUND_STATE_SUCCEEDED, ProviderRefundReference: "mock_re_1", RefundedAmount: req.GetAmount()}, nil
	}
	return f.refund(req)
}

func (f *fakeProvider) GetPaymentStatus(_ context.Context, req *providerv1.GetPaymentStatusRequest) (*providerv1.GetPaymentStatusResponse, error) {
	if f.status == nil {
		return &providerv1.GetPaymentStatusResponse{Result: &providerv1.ProviderResult{Success: true, ProviderReference: req.GetProviderReference()}, Status: providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_NOT_FOUND}, nil
	}
	return f.status(req)
}

type fakeRegistry map[string]*fakeProvider

func (r fakeRegistry) Get(name string) (ProviderClient, bool) {
	p, ok := r[name]
	return p, ok
}

func (r fakeRegistry) Names() []string {
	out := make([]string, 0, len(r))
	for k := range r {
		out = append(out, k)
	}
	return out
}

// ---- 回應產生器 ----

func approved(capture bool) func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
	return func(req *providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
		st := providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_AUTHORIZED
		captured := &commonv1.Money{AmountMinor: 0, Currency: req.GetAmount().GetCurrency()}
		if capture {
			st = providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED
			captured = req.GetAmount()
		}
		return &providerv1.AuthorizeResponse{
			Result: &providerv1.ProviderResult{Success: true, ProviderReference: "ref_" + req.GetIdempotencyKey()}, Status: st,
			AuthorizedAmount: req.GetAmount(), CapturedAmount: captured, Fee: &commonv1.Money{AmountMinor: 59, Currency: req.GetAmount().GetCurrency()},
			AuthorizationExpiresAt: timestamppb.New(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)),
			InstrumentDetails:      &providerv1.InstrumentDetails{Brand: "visa", Last4: "4242"},
		}, nil
	}
}

func declined(cat providerv1.ProviderErrorCategory, code string) func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
	return func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
		return &providerv1.AuthorizeResponse{Status: providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED, Result: &providerv1.ProviderResult{Success: false, ErrorCategory: cat, ProviderErrorCode: code, ProviderErrorMessage: "declined: " + code}}, nil
	}
}

func transportErr(code codes.Code) func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
	return func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
		return nil, status.Error(code, "simulated "+code.String())
	}
}

func requiresAction() func(*providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
	return func(req *providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
		return &providerv1.AuthorizeResponse{
			Result: &providerv1.ProviderResult{Success: true, ProviderReference: "ref_3ds"}, Status: providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_REQUIRES_ACTION,
			NextAction: &providerv1.NextAction{Action: &providerv1.NextAction_Redirect{Redirect: &providerv1.RedirectAction{Url: "http://mock/3ds/x", Method: "GET"}}, ExpiresAt: timestamppb.New(time.Date(2026, 8, 20, 10, 30, 0, 0, time.UTC))},
		}, nil
	}
}

var errBoom = errors.New("boom")

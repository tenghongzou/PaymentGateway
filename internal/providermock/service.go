package providermock

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

// ProviderName 為本 adapter 的識別碼。
const ProviderName = "mock"

// Config 為 mock 設定。
type Config struct {
	// DefaultLatency 為每次呼叫的基礎延遲（PG_MOCK_DEFAULT_LATENCY）。
	DefaultLatency time.Duration
	// WebhookSecret 為 ParseWebhook 驗簽用的 HMAC secret（PG_MOCK_WEBHOOK_SECRET）。
	WebhookSecret string
	// BaseURL 為 3DS redirect URL 的主機（PG_MOCK_BASE_URL，預設 http://provider-mock:9101）。
	BaseURL string
	// FeeBps / FeeFixed 為模擬手續費（預設 290 bps + 30）。
	FeeBps   int64
	FeeFixed int64
	// AuthValidity 為預設授權有效期（7 天）。
	AuthValidity time.Duration
	Version      string
	Logger       *slog.Logger
}

// Service 實作 pg.provider.v1.ProviderAdapter。
type Service struct {
	providerv1.UnimplementedProviderAdapterServer
	cfg   Config
	store *Store
	calls atomic.Int64
	now   func() time.Time
}

// NewService 建立 mock 服務。
func NewService(cfg Config) *Service {
	if cfg.WebhookSecret == "" {
		cfg.WebhookSecret = "mock_webhook_secret"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://provider-mock:9101"
	}
	if cfg.FeeBps == 0 {
		cfg.FeeBps = 290
	}
	if cfg.FeeFixed == 0 {
		cfg.FeeFixed = 30
	}
	if cfg.AuthValidity == 0 {
		cfg.AuthValidity = 7 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Service{cfg: cfg, store: NewStore(), now: time.Now}
}

// Register 把服務註冊到 gRPC server。
func (s *Service) Register(srv *grpc.Server) { providerv1.RegisterProviderAdapterServer(srv, s) }

// Store 回傳內部儲存（測試用）。
func (s *Service) Store() *Store { return s.store }

// Calls 回傳累計呼叫數。
func (s *Service) Calls() int64 { return s.calls.Load() }

func (s *Service) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Service) fee(amount money.Money) *commonv1.Money {
	pct, err := amount.MulBps(s.cfg.FeeBps)
	if err != nil {
		return money.Zero(amount.Currency).ToProto()
	}
	total, err := pct.Add(money.Money{AmountMinor: s.cfg.FeeFixed, Currency: amount.Currency})
	if err != nil {
		return pct.ToProto()
	}
	return total.ToProto()
}

func okResult(ref string) *providerv1.ProviderResult {
	return &providerv1.ProviderResult{Success: true, ProviderReference: ref}
}

func failResult(ref string, cat providerv1.ProviderErrorCategory, code, msg string, retryable bool) *providerv1.ProviderResult {
	return &providerv1.ProviderResult{Success: false, ProviderReference: ref, ErrorCategory: cat, ProviderErrorCode: code, ProviderErrorMessage: msg, Retryable: retryable}
}

func tokenOf(req *providerv1.AuthorizeRequest) string {
	switch inst := req.GetInstrument().GetInstrument().(type) {
	case *providerv1.PaymentInstrument_CardToken:
		return inst.CardToken.GetToken()
	case *providerv1.PaymentInstrument_Wallet:
		return "wallet_" + strings.ToLower(inst.Wallet.GetType().String())
	case *providerv1.PaymentInstrument_BankTransfer:
		return "bank_transfer"
	default:
		return ""
	}
}

// Authorize 依 token 情境回應（docs/09 §3.1）。
func (s *Service) Authorize(ctx context.Context, req *providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
	s.calls.Add(1)
	amount, err := money.FromProto(req.GetAmount())
	if err != nil {
		return &providerv1.AuthorizeResponse{
			Status: providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED,
			Result: failResult("", providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "invalid_amount", err.Error(), false),
		}, nil
	}
	token := tokenOf(req)
	if token == "" {
		return &providerv1.AuthorizeResponse{
			Status: providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED,
			Result: failResult("", providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "instrument_missing", "payment instrument is required", false),
		}, nil
	}
	// PSP 冪等：同 idempotency_key 回同一筆結果。
	if ref, ok := s.store.IdemGet(req.GetIdempotencyKey()); ok {
		if t, ok := s.store.Get(ref); ok {
			return s.authorizeResponseFor(t), nil
		}
	}
	sc := ScenarioFor(token)
	if err := s.sleep(ctx, s.cfg.DefaultLatency+sc.Latency); err != nil {
		return nil, status.Error(codes.DeadlineExceeded, "mock: canceled while simulating latency")
	}

	switch sc.Behavior {
	case BehaveTimeout:
		// 睡到 deadline 為止，讓呼叫端得到 DEADLINE_EXCEEDED（結果不明；GetPaymentStatus 回 NOT_FOUND）。
		<-ctx.Done()
		return nil, status.Error(codes.DeadlineExceeded, "mock: simulated timeout")
	case BehaveUnavailable:
		return nil, status.Error(codes.Unavailable, "mock: simulated provider outage")
	case BehaveUnavailableOnce:
		if s.store.CountToken(req.GetPaymentId(), token) == 1 {
			return nil, status.Error(codes.Unavailable, "mock: simulated one-off provider outage")
		}
	case BehaveRateLimited:
		return &providerv1.AuthorizeResponse{
			Status: providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED,
			Result: failResult("", sc.Category, sc.Code, sc.Message, true),
		}, nil
	case BehaveDecline, BehaveInvalid:
		return &providerv1.AuthorizeResponse{
			Status: providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED,
			Result: failResult("", sc.Category, sc.Code, sc.Message, sc.Retryable),
		}, nil
	case BehaveApprove, BehaveRequiresAction:
	}

	now := s.now()
	validity := s.cfg.AuthValidity
	if sc.AuthValidity > 0 {
		validity = sc.AuthValidity
	}
	t := &Txn{
		Reference: "mock_pi_" + strings.ToLower(ids.New("m")[2:]), PaymentID: req.GetPaymentId(), Token: token,
		Amount: amount, Captured: money.Zero(amount.Currency), Refunded: money.Zero(amount.Currency),
		AuthExpiry: now.Add(validity), CreatedAt: now, UpdatedAt: now, Refunds: map[string]string{},
	}
	switch {
	case sc.Behavior == BehaveRequiresAction:
		t.Status = TxnRequiresAction
	case req.GetCaptureImmediately():
		t.Status = TxnCaptured
		t.Captured = amount
	default:
		t.Status = TxnAuthorized
	}
	s.store.Put(t)
	s.store.IdemPut(req.GetIdempotencyKey(), t.Reference)
	return s.authorizeResponseFor(*t), nil
}

func (s *Service) authorizeResponseFor(t Txn) *providerv1.AuthorizeResponse {
	resp := &providerv1.AuthorizeResponse{
		Result:                 okResult(t.Reference),
		AuthorizedAmount:       t.Amount.ToProto(),
		CapturedAmount:         t.Captured.ToProto(),
		AuthorizationExpiresAt: timestamppb.New(t.AuthExpiry),
		Fee:                    s.fee(t.Amount),
		InstrumentDetails:      &providerv1.InstrumentDetails{Type: "card", Brand: "visa", Last4: "4242", Funding: "credit", IssuerCountry: "TW", ThreeDsResult: "not_supported"},
	}
	switch t.Status {
	case TxnRequiresAction:
		resp.Status = providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_REQUIRES_ACTION
		resp.AuthorizedAmount = money.Zero(t.Amount.Currency).ToProto()
		resp.NextAction = &providerv1.NextAction{
			Action:    &providerv1.NextAction_Redirect{Redirect: &providerv1.RedirectAction{Url: s.cfg.BaseURL + "/3ds/" + t.Reference, Method: "GET"}},
			ExpiresAt: timestamppb.New(t.CreatedAt.Add(30 * time.Minute)),
		}
		resp.InstrumentDetails.ThreeDsResult = "pending"
	case TxnCaptured:
		resp.Status = providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED
	case TxnAuthorized, TxnVoided, TxnRefunded, TxnPartialRefund, TxnFailed:
		resp.Status = providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_AUTHORIZED
	}
	return resp
}

// Capture 請款：authorized（或 requires_action，視同 3DS 已完成）→ captured。
func (s *Service) Capture(ctx context.Context, req *providerv1.CaptureRequest) (*providerv1.CaptureResponse, error) {
	s.calls.Add(1)
	if err := s.sleep(ctx, s.cfg.DefaultLatency); err != nil {
		return nil, status.Error(codes.DeadlineExceeded, "mock: canceled")
	}
	t, ok := s.store.Get(req.GetProviderReference())
	if !ok {
		return &providerv1.CaptureResponse{Result: failResult(req.GetProviderReference(), providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "not_found", "unknown provider_reference", false)}, nil
	}
	if ScenarioFor(t.Token).CaptureFails {
		return nil, status.Error(codes.Unavailable, "mock: simulated capture outage")
	}
	amount := t.Amount
	if req.GetAmount() != nil {
		m, err := money.FromProto(req.GetAmount())
		if err != nil || m.Currency != t.Amount.Currency {
			return &providerv1.CaptureResponse{Result: failResult(t.Reference, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "invalid_amount", "amount/currency invalid", false)}, nil //nolint:nilerr // adapter 契約：業務拒絕以 ProviderResult 表達，不回 gRPC error
		}
		amount = m
	}
	switch t.Status {
	case TxnCaptured:
		// 冪等：已請款直接回成功。
		return &providerv1.CaptureResponse{Result: okResult(t.Reference), CapturedAmount: t.Captured.ToProto(), Fee: s.fee(t.Captured)}, nil
	case TxnAuthorized, TxnRequiresAction:
	case TxnVoided, TxnRefunded, TxnPartialRefund, TxnFailed:
		return &providerv1.CaptureResponse{Result: failResult(t.Reference, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "invalid_state", fmt.Sprintf("cannot capture a %s transaction", t.Status), false)}, nil
	}
	if amount.GreaterThan(t.Amount) {
		return &providerv1.CaptureResponse{Result: failResult(t.Reference, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "amount_too_large", "capture exceeds authorized amount", false)}, nil
	}
	s.store.Update(t.Reference, func(x *Txn) {
		x.Status = TxnCaptured
		x.Captured = amount
	})
	return &providerv1.CaptureResponse{Result: okResult(t.Reference), CapturedAmount: amount.ToProto(), Fee: s.fee(amount)}, nil
}

// Void 取消授權。
func (s *Service) Void(ctx context.Context, req *providerv1.VoidRequest) (*providerv1.VoidResponse, error) {
	s.calls.Add(1)
	if err := s.sleep(ctx, s.cfg.DefaultLatency); err != nil {
		return nil, status.Error(codes.DeadlineExceeded, "mock: canceled")
	}
	t, ok := s.store.Get(req.GetProviderReference())
	if !ok {
		return &providerv1.VoidResponse{Result: failResult(req.GetProviderReference(), providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "not_found", "unknown provider_reference", false)}, nil
	}
	switch t.Status {
	case TxnVoided:
		return &providerv1.VoidResponse{Result: okResult(t.Reference), VoidedAmount: t.Amount.ToProto()}, nil
	case TxnAuthorized, TxnRequiresAction:
	case TxnCaptured, TxnRefunded, TxnPartialRefund, TxnFailed:
		return &providerv1.VoidResponse{Result: failResult(t.Reference, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "invalid_state", fmt.Sprintf("cannot void a %s transaction", t.Status), false)}, nil
	}
	s.store.Update(t.Reference, func(x *Txn) { x.Status = TxnVoided })
	return &providerv1.VoidResponse{Result: okResult(t.Reference), VoidedAmount: t.Amount.ToProto()}, nil
}

// Refund 退款（可部分、可多次；以 refund_id 冪等）。
func (s *Service) Refund(ctx context.Context, req *providerv1.RefundRequest) (*providerv1.RefundResponse, error) {
	s.calls.Add(1)
	if err := s.sleep(ctx, s.cfg.DefaultLatency); err != nil {
		return nil, status.Error(codes.DeadlineExceeded, "mock: canceled")
	}
	t, ok := s.store.Get(req.GetProviderReference())
	if !ok {
		return &providerv1.RefundResponse{Status: providerv1.RefundState_REFUND_STATE_FAILED, Result: failResult(req.GetProviderReference(), providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "not_found", "unknown provider_reference", false)}, nil
	}
	if ScenarioFor(t.Token).RefundFails {
		return &providerv1.RefundResponse{Status: providerv1.RefundState_REFUND_STATE_FAILED, Result: failResult(t.Reference, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD, "refund_declined", "issuer declined the refund", false)}, nil
	}
	if ref, ok := t.Refunds[req.GetRefundId()]; ok {
		return &providerv1.RefundResponse{Status: providerv1.RefundState_REFUND_STATE_SUCCEEDED, Result: okResult(t.Reference), ProviderRefundReference: ref, RefundedAmount: req.GetAmount()}, nil
	}
	amount, err := money.FromProto(req.GetAmount())
	if err != nil || amount.Currency != t.Amount.Currency || !amount.IsPositive() {
		return &providerv1.RefundResponse{Status: providerv1.RefundState_REFUND_STATE_FAILED, Result: failResult(t.Reference, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "invalid_amount", "amount/currency invalid", false)}, nil //nolint:nilerr // adapter 契約：業務拒絕以 ProviderResult 表達，不回 gRPC error
	}
	switch t.Status {
	case TxnCaptured, TxnPartialRefund:
	case TxnAuthorized, TxnRequiresAction, TxnVoided, TxnRefunded, TxnFailed:
		return &providerv1.RefundResponse{Status: providerv1.RefundState_REFUND_STATE_FAILED, Result: failResult(t.Reference, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "invalid_state", fmt.Sprintf("cannot refund a %s transaction", t.Status), false)}, nil
	}
	newRefunded, err := t.Refunded.Add(amount)
	if err != nil || newRefunded.GreaterThan(t.Captured) {
		return &providerv1.RefundResponse{Status: providerv1.RefundState_REFUND_STATE_FAILED, Result: failResult(t.Reference, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, "amount_too_large", "refund exceeds captured amount", false)}, nil //nolint:nilerr // adapter 契約：業務拒絕以 ProviderResult 表達，不回 gRPC error
	}
	ref := "mock_re_" + strings.ToLower(ids.New("m")[2:])
	s.store.Update(t.Reference, func(x *Txn) {
		x.Refunded = newRefunded
		x.Status = TxnPartialRefund
		if newRefunded.Equal(x.Captured) {
			x.Status = TxnRefunded
		}
		x.Refunds[req.GetRefundId()] = ref
	})
	return &providerv1.RefundResponse{Status: providerv1.RefundState_REFUND_STATE_SUCCEEDED, Result: okResult(t.Reference), ProviderRefundReference: ref, RefundedAmount: amount.ToProto(), Fee: money.Zero(amount.Currency).ToProto()}, nil
}

// GetPaymentStatus 查詢狀態；requires_action 的交易在此視為「持卡人已完成 3DS」並轉為 authorized
// （供 ConfirmPayment 使用；docs/09 §3.1 tok_3ds 的後續行為）。
func (s *Service) GetPaymentStatus(ctx context.Context, req *providerv1.GetPaymentStatusRequest) (*providerv1.GetPaymentStatusResponse, error) {
	s.calls.Add(1)
	if err := s.sleep(ctx, s.cfg.DefaultLatency); err != nil {
		return nil, status.Error(codes.DeadlineExceeded, "mock: canceled")
	}
	t, ok := s.store.Get(req.GetProviderReference())
	if !ok {
		return &providerv1.GetPaymentStatusResponse{Result: okResult(req.GetProviderReference()), Status: providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_NOT_FOUND, UpdatedAt: timestamppb.Now()}, nil
	}
	if t.Status == TxnRequiresAction {
		s.store.Update(t.Reference, func(x *Txn) { x.Status = TxnAuthorized })
		t.Status = TxnAuthorized
	}
	var st providerv1.ProviderPaymentStatus
	switch t.Status {
	case TxnRequiresAction:
		st = providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_REQUIRES_ACTION
	case TxnAuthorized:
		st = providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_AUTHORIZED
	case TxnCaptured:
		st = providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_CAPTURED
	case TxnVoided:
		st = providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_VOIDED
	case TxnRefunded:
		st = providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_REFUNDED
	case TxnPartialRefund:
		st = providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_PARTIALLY_REFUNDED
	case TxnFailed:
		st = providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_FAILED
	}
	return &providerv1.GetPaymentStatusResponse{
		Result: okResult(t.Reference), Status: st,
		AuthorizedAmount: t.Amount.ToProto(), CapturedAmount: t.Captured.ToProto(), RefundedAmount: t.Refunded.ToProto(),
		UpdatedAt: timestamppb.New(t.UpdatedAt),
	}, nil
}

// webhookPayload 為 mock webhook 的 JSON 格式。
type webhookPayload struct {
	ID                string `json:"id"`
	Type              string `json:"type"`
	ProviderReference string `json:"provider_reference"`
	PaymentID         string `json:"payment_id,omitempty"`
	RefundID          string `json:"refund_id,omitempty"`
	AmountMinor       int64  `json:"amount_minor,omitempty"`
	Currency          string `json:"currency,omitempty"`
	OccurredAt        string `json:"occurred_at,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
	DisputeReference  string `json:"dispute_reference,omitempty"`
	DisputeReason     string `json:"dispute_reason,omitempty"`
}

var webhookTypes = map[string]providerv1.ProviderWebhookEventType{
	"authorization.succeeded": providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_AUTHORIZATION_SUCCEEDED,
	"authorization.failed":    providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_AUTHORIZATION_FAILED,
	"capture.succeeded":       providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_CAPTURE_SUCCEEDED,
	"capture.failed":          providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_CAPTURE_FAILED,
	"voided":                  providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_VOIDED,
	"refund.succeeded":        providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_REFUND_SUCCEEDED,
	"refund.failed":           providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_REFUND_FAILED,
	"dispute.opened":          providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_DISPUTE_OPENED,
	"dispute.updated":         providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_DISPUTE_UPDATED,
	"dispute.closed":          providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_DISPUTE_CLOSED,
	"payment.expired":         providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_PAYMENT_EXPIRED,
}

// SignWebhook 產生 X-Mock-Signature（測試與 mock 自己發 webhook 時使用）。
func (s *Service) SignWebhook(ts int64, body []byte) string {
	return sig.SignWebhook(s.cfg.WebhookSecret, ts, body)
}

// ParseWebhook 驗證 X-Mock-Signature（t=..,v1=..）並把 JSON 正規化為 ProviderWebhookEvent。
func (s *Service) ParseWebhook(_ context.Context, req *providerv1.ParseWebhookRequest) (*providerv1.ParseWebhookResponse, error) {
	s.calls.Add(1)
	var header string
	for k, v := range req.GetHeaders() {
		if strings.EqualFold(k, "X-Mock-Signature") && len(v.GetValues()) > 0 {
			header = v.GetValues()[0]
		}
	}
	if header == "" {
		return &providerv1.ParseWebhookResponse{Verified: false, RejectionReason: "missing X-Mock-Signature header"}, nil
	}
	if err := sig.VerifyWebhook([]string{s.cfg.WebhookSecret}, header, req.GetBody(), s.now(), sig.DefaultWindow); err != nil {
		return &providerv1.ParseWebhookResponse{Verified: false, RejectionReason: err.Error()}, nil //nolint:nilerr // 驗簽失敗以 verified=false 表達
	}
	var p webhookPayload
	if err := json.Unmarshal(req.GetBody(), &p); err != nil {
		return &providerv1.ParseWebhookResponse{Verified: true, Ignored: true, RejectionReason: "malformed JSON body"}, nil //nolint:nilerr // 無法解析以 ignored=true 表達
	}
	et, ok := webhookTypes[p.Type]
	if !ok {
		return &providerv1.ParseWebhookResponse{Verified: true, Ignored: true, Event: &providerv1.ProviderWebhookEvent{
			ProviderEventId: p.ID, EventType: providerv1.ProviderWebhookEventType_PROVIDER_WEBHOOK_EVENT_TYPE_UNKNOWN, ProviderEventType: p.Type, ProviderReference: p.ProviderReference,
		}}, nil
	}
	ev := &providerv1.ProviderWebhookEvent{
		ProviderEventId: p.ID, EventType: et, ProviderEventType: p.Type, ProviderReference: p.ProviderReference,
		PaymentId: p.PaymentID, RefundId: p.RefundID, ProviderErrorCode: p.ErrorCode, ProviderErrorMessage: p.ErrorMessage, TestMode: true,
	}
	if p.ID == "" {
		ev.ProviderEventId = "mock_evt_" + ids.New("m")[2:]
	}
	if p.Currency != "" {
		ev.Amount = &commonv1.Money{AmountMinor: p.AmountMinor, Currency: p.Currency}
	}
	if t, err := time.Parse(time.RFC3339, p.OccurredAt); err == nil {
		ev.OccurredAt = timestamppb.New(t)
	} else {
		ev.OccurredAt = timestamppb.New(s.now())
	}
	if p.DisputeReference != "" {
		ev.Dispute = &providerv1.DisputeInfo{ProviderDisputeReference: p.DisputeReference, Reason: p.DisputeReason}
	}
	if t, ok := s.store.Get(p.ProviderReference); ok && p.PaymentID == "" {
		ev.PaymentId = t.PaymentID
	}
	return &providerv1.ParseWebhookResponse{Verified: true, Event: ev}, nil
}

// HealthCheck 回報 SERVING 與能力宣告。
func (s *Service) HealthCheck(_ context.Context, _ *providerv1.HealthCheckRequest) (*providerv1.HealthCheckResponse, error) {
	s.calls.Add(1)
	return &providerv1.HealthCheckResponse{
		Status: providerv1.HealthStatus_HEALTH_STATUS_SERVING, Provider: ProviderName, AdapterVersion: s.cfg.Version,
		ProviderLatencyMs: s.cfg.DefaultLatency.Milliseconds(), CheckedAt: timestamppb.New(s.now()),
		Capabilities: &providerv1.ProviderCapabilities{
			PartialCapture: true, MultiCapture: false, PartialRefund: true, ThreeDs: true,
			InstrumentTypes:           []string{"card", "wallet"},
			Currencies:                []string{"TWD", "USD", "EUR", "JPY", "GBP", "SGD", "HKD", "KRW", "CNY"},
			WalletTypes:               []providerv1.WalletType{providerv1.WalletType_WALLET_TYPE_APPLE_PAY, providerv1.WalletType_WALLET_TYPE_GOOGLE_PAY},
			AuthorizationValidityDays: 7,
		},
	}, nil
}

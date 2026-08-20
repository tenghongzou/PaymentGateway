// Package grpc 實作 pg.payment.v1.PaymentService。
package grpc

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/app"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// metadataRequestHash 為 gateway 傳入的 request hash（docs/05 §10.3）。
const metadataRequestHash = "x-pg-request-hash"

// Server 實作 PaymentService。
// TODO：ListRefunds / GetDispute / ListDisputes / SubmitDisputeEvidence 目前由內嵌的 Unimplemented 回 codes.Unimplemented。
type Server struct {
	paymentv1.UnimplementedPaymentServiceServer
	svc *app.Service
}

// NewServer 建立 Server。
func NewServer(svc *app.Service) *Server { return &Server{svc: svc} }

// Register 註冊到 gRPC server。
func (s *Server) Register(srv *grpc.Server) { paymentv1.RegisterPaymentServiceServer(srv, s) }

func requestHash(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(metadataRequestHash); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// CreatePayment 建立付款。
func (s *Server) CreatePayment(ctx context.Context, req *paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error) {
	cmd, err := toCreateCommand(req, requestHash(ctx))
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	res, err := s.svc.CreatePayment(ctx, cmd)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &paymentv1.CreatePaymentResponse{Payment: PaymentToProto(res.Payment), IdempotentReplayed: res.Replayed}, nil
}

// GetPayment 查詢付款。
func (s *Server) GetPayment(ctx context.Context, req *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
	p, err := s.svc.GetPayment(ctx, req.GetMerchantId(), req.GetPaymentId())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &paymentv1.GetPaymentResponse{Payment: PaymentToProto(p)}, nil
}

// ListPayments 列出付款。
func (s *Server) ListPayments(ctx context.Context, req *paymentv1.ListPaymentsRequest) (*paymentv1.ListPaymentsResponse, error) {
	f := app.ListFilter{Limit: int(req.GetPage().GetPageSize()), Cursor: req.GetPage().GetPageToken(), CustomerID: req.GetCustomerId(), Provider: req.GetProvider(), Currency: req.GetCurrency()}
	for _, st := range req.GetStatuses() {
		if d := app.StatusFromProto(st); d != "" {
			f.Statuses = append(f.Statuses, d)
		}
	}
	if req.GetCreatedAfter() != nil {
		t := req.GetCreatedAfter().AsTime()
		f.CreatedAfter = &t
	}
	if req.GetCreatedBefore() != nil {
		t := req.GetCreatedBefore().AsTime()
		f.CreatedBefore = &t
	}
	items, next, err := s.svc.ListPayments(ctx, req.GetMerchantId(), f)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	out := &paymentv1.ListPaymentsResponse{Page: &commonv1.PageResponse{NextPageToken: next, HasMore: next != ""}}
	for _, p := range items {
		out.Payments = append(out.Payments, PaymentToProto(p))
	}
	return out, nil
}

// CapturePayment 請款。
func (s *Server) CapturePayment(ctx context.Context, req *paymentv1.CapturePaymentRequest) (*paymentv1.CapturePaymentResponse, error) {
	cmd := app.CaptureCommand{MerchantID: req.GetMerchantId(), PaymentID: req.GetPaymentId(), IdempotencyKey: req.GetIdempotencyKey(), Final: true}
	if req.Final != nil {
		cmd.Final = req.GetFinal()
	}
	if req.GetAmount() != nil {
		m, err := money.FromProto(req.GetAmount())
		if err != nil {
			return nil, grpcx.ErrorFromDomain(domain.ErrInvalidCurrency.Wrap(err))
		}
		cmd.Amount = &m
	}
	p, err := s.svc.CapturePayment(ctx, cmd)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &paymentv1.CapturePaymentResponse{Payment: PaymentToProto(p)}, nil
}

// VoidPayment 取消授權。
func (s *Server) VoidPayment(ctx context.Context, req *paymentv1.VoidPaymentRequest) (*paymentv1.VoidPaymentResponse, error) {
	p, err := s.svc.VoidPayment(ctx, app.VoidCommand{MerchantID: req.GetMerchantId(), PaymentID: req.GetPaymentId(), IdempotencyKey: req.GetIdempotencyKey(), Reason: req.GetReason()})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &paymentv1.VoidPaymentResponse{Payment: PaymentToProto(p)}, nil
}

// ConfirmPayment 3DS 後確認。
func (s *Server) ConfirmPayment(ctx context.Context, req *paymentv1.ConfirmPaymentRequest) (*paymentv1.ConfirmPaymentResponse, error) {
	p, err := s.svc.ConfirmPayment(ctx, app.ConfirmCommand{MerchantID: req.GetMerchantId(), PaymentID: req.GetPaymentId(), IdempotencyKey: req.GetIdempotencyKey(), ProviderParams: req.GetProviderParams()})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &paymentv1.ConfirmPaymentResponse{Payment: PaymentToProto(p)}, nil
}

// CreateRefund 建立退款。
func (s *Server) CreateRefund(ctx context.Context, req *paymentv1.CreateRefundRequest) (*paymentv1.CreateRefundResponse, error) {
	cmd := app.CreateRefundCommand{MerchantID: req.GetMerchantId(), PaymentID: req.GetPaymentId(), IdempotencyKey: req.GetIdempotencyKey(), RequestHash: requestHash(ctx), Reason: req.GetReason(), Metadata: req.GetMetadata()}
	if req.GetAmount() != nil {
		m, err := money.FromProto(req.GetAmount())
		if err != nil {
			return nil, grpcx.ErrorFromDomain(domain.ErrInvalidCurrency.Wrap(err))
		}
		cmd.Amount = &m
	}
	res, err := s.svc.CreateRefund(ctx, cmd)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &paymentv1.CreateRefundResponse{Refund: RefundToProto(res.Refund), IdempotentReplayed: res.Replayed}, nil
}

// GetRefund 查詢退款。
func (s *Server) GetRefund(ctx context.Context, req *paymentv1.GetRefundRequest) (*paymentv1.GetRefundResponse, error) {
	r, err := s.svc.GetRefund(ctx, req.GetMerchantId(), req.GetRefundId())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &paymentv1.GetRefundResponse{Refund: RefundToProto(r)}, nil
}

// ---- 轉換 ----

func toCreateCommand(req *paymentv1.CreatePaymentRequest, hash string) (app.CreatePaymentCommand, error) {
	amount, err := money.FromProto(req.GetAmount())
	if err != nil {
		return app.CreatePaymentCommand{}, domain.ErrInvalidCurrency.Wrap(err)
	}
	cmd := app.CreatePaymentCommand{
		MerchantID: req.GetMerchantId(), IdempotencyKey: req.GetIdempotencyKey(), RequestHash: hash, Amount: amount,
		Description: req.GetDescription(), StatementDescriptor: req.GetStatementDescriptor(), ReturnURL: req.GetReturnUrl(),
		Metadata: req.GetMetadata(), LiveMode: req.GetLivemode(), ThreeDS: req.GetThreeDs(), PreferredProvider: req.GetPreferredProvider(),
	}
	if req.GetMerchantId() == "" {
		return cmd, apperr.ErrParameterMissing.WithMessage("merchant_id is required").WithParam("merchant_id")
	}
	switch req.GetCaptureMethod() {
	case paymentv1.CaptureMethod_CAPTURE_METHOD_MANUAL:
		cmd.CaptureMethod = domain.CaptureManual
	case paymentv1.CaptureMethod_CAPTURE_METHOD_AUTOMATIC, paymentv1.CaptureMethod_CAPTURE_METHOD_UNSPECIFIED:
		cmd.CaptureMethod = domain.CaptureAutomatic
	}
	if c := req.GetCustomer(); c != nil {
		cmd.Customer = domain.Customer{ID: c.GetId(), Email: c.GetEmail(), Name: c.GetName(), Phone: c.GetPhone(), IPAddress: c.GetIpAddress(), UserAgent: c.GetUserAgent(), BillingCountry: c.GetBillingCountry(), BillingPostalCode: c.GetBillingPostalCode()}
	}
	switch m := req.GetPaymentMethod().GetMethod().(type) {
	case *paymentv1.PaymentMethod_CardToken:
		if m.CardToken.GetToken() == "" {
			return cmd, domain.ErrPaymentMethodInvalid.WithMessage("card token is required").WithParam("payment_method.card.token")
		}
		if isPANLike(m.CardToken.GetToken()) {
			return cmd, domain.ErrPANNotAllowed
		}
		cmd.PaymentMethodType = "card"
		cmd.MethodDetails = domain.PaymentMethodDetails{Brand: m.CardToken.GetBrand(), Last4: m.CardToken.GetLast4(), ExpMonth: int(m.CardToken.GetExpMonth()), ExpYear: int(m.CardToken.GetExpYear()), TokenProvider: m.CardToken.GetTokenProvider()}
		cmd.Instrument = &providerv1.PaymentInstrument{Instrument: &providerv1.PaymentInstrument_CardToken{CardToken: &providerv1.CardToken{
			Token: m.CardToken.GetToken(), TokenProvider: m.CardToken.GetTokenProvider(), Brand: m.CardToken.GetBrand(), Last4: m.CardToken.GetLast4(),
			ExpMonth: m.CardToken.GetExpMonth(), ExpYear: m.CardToken.GetExpYear(), HolderName: m.CardToken.GetHolderName(),
		}}}
	case *paymentv1.PaymentMethod_Wallet:
		cmd.PaymentMethodType = "wallet"
		cmd.MethodDetails = domain.PaymentMethodDetails{WalletType: m.Wallet.GetType()}
		cmd.Instrument = &providerv1.PaymentInstrument{Instrument: &providerv1.PaymentInstrument_Wallet{Wallet: &providerv1.WalletPayment{Type: walletType(m.Wallet.GetType()), EncryptedPayload: m.Wallet.GetEncryptedPayload()}}}
	case *paymentv1.PaymentMethod_BankTransfer:
		cmd.PaymentMethodType = "bank_transfer"
		cmd.MethodDetails = domain.PaymentMethodDetails{BankCountry: m.BankTransfer.GetCountry(), BankCode: m.BankTransfer.GetBankCode()}
		cmd.Instrument = &providerv1.PaymentInstrument{Instrument: &providerv1.PaymentInstrument_BankTransfer{BankTransfer: &providerv1.BankTransfer{Country: m.BankTransfer.GetCountry(), BankCode: m.BankTransfer.GetBankCode(), PayerName: m.BankTransfer.GetPayerName()}}}
	default:
		return cmd, domain.ErrPaymentMethodMissing
	}
	return cmd, nil
}

func isPANLike(s string) bool {
	if len(s) < 13 || len(s) > 19 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func walletType(s string) providerv1.WalletType {
	switch strings.ToLower(s) {
	case "apple_pay":
		return providerv1.WalletType_WALLET_TYPE_APPLE_PAY
	case "google_pay":
		return providerv1.WalletType_WALLET_TYPE_GOOGLE_PAY
	case "line_pay":
		return providerv1.WalletType_WALLET_TYPE_LINE_PAY
	default:
		return providerv1.WalletType_WALLET_TYPE_UNSPECIFIED
	}
}

func attemptStatusToProto(s domain.AttemptStatus) paymentv1.AttemptStatus {
	switch s {
	case domain.AttemptPending:
		return paymentv1.AttemptStatus_ATTEMPT_STATUS_PENDING
	case domain.AttemptRequiresAction:
		return paymentv1.AttemptStatus_ATTEMPT_STATUS_REQUIRES_ACTION
	case domain.AttemptApproved:
		return paymentv1.AttemptStatus_ATTEMPT_STATUS_APPROVED
	case domain.AttemptDeclined:
		return paymentv1.AttemptStatus_ATTEMPT_STATUS_DECLINED
	case domain.AttemptUnavailable:
		return paymentv1.AttemptStatus_ATTEMPT_STATUS_UNAVAILABLE
	case domain.AttemptUnknown:
		return paymentv1.AttemptStatus_ATTEMPT_STATUS_UNKNOWN
	default:
		return paymentv1.AttemptStatus_ATTEMPT_STATUS_UNSPECIFIED
	}
}

// PaymentToProto 把領域 Payment 轉成 proto。
func PaymentToProto(p *domain.Payment) *paymentv1.Payment {
	if p == nil {
		return nil
	}
	out := &paymentv1.Payment{
		Id: p.PublicID, MerchantId: p.MerchantID, Amount: p.Amount.ToProto(), CapturedAmount: p.AmountCaptured.ToProto(), RefundedAmount: p.AmountRefunded.ToProto(),
		Status: app.StatusToProto(p.Status), CaptureMethod: app.CaptureMethodToProto(p.CaptureMethod), PaymentMethodType: app.MethodTypeToProto(p.PaymentMethodType),
		PaymentMethodDetails: app.DetailsToProto(p),
		Customer:             &paymentv1.Customer{Id: p.Customer.ID, Email: p.Customer.Email, Name: p.Customer.Name, Phone: p.Customer.Phone, BillingCountry: p.Customer.BillingCountry, BillingPostalCode: p.Customer.BillingPostalCode},
		Metadata:             p.Metadata, Description: p.Description, ReturnUrl: p.ReturnURL, StatementDescriptor: p.StatementDescriptor,
		Provider: p.SelectedProvider, ProviderReference: p.ProviderReference, IdempotencyKey: p.IdempotencyKey, Livemode: p.LiveMode,
		CreatedAt: timestamppb.New(p.CreatedAt), UpdatedAt: timestamppb.New(p.UpdatedAt),
	}
	if p.AuthorizedAt != nil {
		out.AuthorizedAt = timestamppb.New(*p.AuthorizedAt)
	}
	if p.CapturedAt != nil {
		out.CapturedAt = timestamppb.New(*p.CapturedAt)
	}
	switch {
	case p.Status == domain.StatusAuthorized && p.AuthExpiresAt != nil:
		out.ExpiresAt = timestamppb.New(*p.AuthExpiresAt)
	case p.ExpiresAt != nil:
		out.ExpiresAt = timestamppb.New(*p.ExpiresAt)
	}
	if p.Failure != nil {
		out.LastError = &commonv1.ErrorDetail{Type: apperr.TypeProvider, Code: p.Failure.PublicCode(), Message: p.Failure.Message}
		if p.Failure.Category.RESTCode() != "card_declined" {
			out.LastError.Code = p.Failure.Category.RESTCode()
		}
	}
	for _, a := range p.Attempts {
		pa := &paymentv1.PaymentAttempt{
			Id: a.PublicID(), Sequence: int32(a.AttemptNo), Provider: a.Provider, ProviderReference: a.ProviderReference, //nolint:gosec // ≤ 3
			Status: attemptStatusToProto(a.Status), ErrorCode: a.ErrorCode, ErrorMessage: a.ErrorMessage, RoutingReason: a.RouteReason,
			CreatedAt: timestamppb.New(a.CreatedAt),
		}
		if a.ErrorCategory != domain.CategoryNone {
			pa.ErrorCategory = app.CategoryToProto(a.ErrorCategory)
		}
		if a.CompletedAt != nil {
			pa.CompletedAt = timestamppb.New(*a.CompletedAt)
		}
		out.Attempts = append(out.Attempts, pa)
		if p.Status == domain.StatusRequiresAction && a.Status == domain.AttemptRequiresAction && a.NextAction != nil {
			out.NextAction = nextActionToProto(a.NextAction)
		}
	}
	return out
}

func nextActionToProto(n *domain.NextAction) *paymentv1.NextAction {
	out := &paymentv1.NextAction{}
	if !n.ExpiresAt.IsZero() {
		out.ExpiresAt = timestamppb.New(n.ExpiresAt)
	}
	switch n.Type {
	case "three_ds_challenge":
		out.Action = &paymentv1.NextAction_ThreeDsChallenge{ThreeDsChallenge: &providerv1.ThreeDsChallengeAction{AcsUrl: n.ACSURL, Creq: n.CReq, TransactionId: n.TxnID, Version: n.Version}}
	case "display":
		out.Action = &paymentv1.NextAction_Display{Display: &providerv1.DisplayAction{Type: n.Display["type"], Details: n.Display}}
	default:
		out.Action = &paymentv1.NextAction_Redirect{Redirect: &providerv1.RedirectAction{Url: n.URL, Method: n.Method, FormFields: n.FormFields}}
	}
	return out
}

// RefundToProto 把領域 Refund 轉成 proto。
func RefundToProto(r *domain.Refund) *paymentv1.Refund {
	if r == nil {
		return nil
	}
	var st paymentv1.RefundStatus
	switch r.Status {
	case domain.RefundPending:
		st = paymentv1.RefundStatus_REFUND_STATUS_PENDING
	case domain.RefundSucceeded:
		st = paymentv1.RefundStatus_REFUND_STATUS_SUCCEEDED
	case domain.RefundFailed:
		st = paymentv1.RefundStatus_REFUND_STATUS_FAILED
	}
	out := &paymentv1.Refund{
		Id: r.PublicID, PaymentId: r.PaymentPublicID, MerchantId: r.MerchantID, Amount: r.Amount.ToProto(), Status: st, Reason: r.Reason,
		Provider: r.Provider, ProviderReference: r.ProviderReference, FailureCode: r.FailureCode, FailureMessage: r.FailureMessage,
		Metadata: r.Metadata, IdempotencyKey: r.IdempotencyKey, Livemode: r.LiveMode, CreatedAt: timestamppb.New(r.CreatedAt), UpdatedAt: timestamppb.New(r.UpdatedAt),
	}
	if r.SucceededAt != nil {
		out.CompletedAt = timestamppb.New(*r.SucceededAt)
	} else if r.Status == domain.RefundFailed {
		out.CompletedAt = timestamppb.New(r.UpdatedAt)
	}
	return out
}

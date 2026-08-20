package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/httpx"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
)

// metadataRequestHash 為傳給 payment-service 的 gRPC metadata（docs/05 §10.3）。
const metadataRequestHash = "x-pg-request-hash"

func (g *Gateway) outgoing(ctx context.Context, r *http.Request) context.Context {
	if h := r.Header.Get(headerRequestHash); h != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, metadataRequestHash, h)
	}
	return ctx
}

func idempotencyKey(r *http.Request) string { return r.Header.Get(httpx.HeaderIdempotencyKey) }

func (g *Gateway) createPayment(w http.ResponseWriter, r *http.Request) {
	principal := PrincipalFromContext(r.Context())
	var in createPaymentJSON
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	req, err := in.toProto(principal, idempotencyKey(r))
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	resp, err := g.payments.CreatePayment(g.outgoing(r.Context(), r), req)
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	if resp.GetIdempotentReplayed() {
		w.Header().Set(httpx.HeaderIdempotentReplayed, "true")
	}
	httpx.WriteJSON(w, http.StatusCreated, paymentToJSON(resp.GetPayment(), principal.LiveMode))
}

func (g *Gateway) getPayment(w http.ResponseWriter, r *http.Request) {
	principal := PrincipalFromContext(r.Context())
	resp, err := g.payments.GetPayment(g.outgoing(r.Context(), r), &paymentv1.GetPaymentRequest{MerchantId: principal.MerchantID, PaymentId: chi.URLParam(r, "id")})
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, paymentToJSON(resp.GetPayment(), principal.LiveMode))
}

func (g *Gateway) listPayments(w http.ResponseWriter, r *http.Request) {
	principal := PrincipalFromContext(r.Context())
	limit, err := httpx.ParseIntQuery(r, "limit", 20)
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	q := r.URL.Query()
	req := &paymentv1.ListPaymentsRequest{
		MerchantId: principal.MerchantID,
		Page:       &commonv1.PageRequest{PageSize: int32(min(max(limit, 1), 100)), PageToken: q.Get("cursor")}, //nolint:gosec // bounded 1..100
		CustomerId: q.Get("customer_id"), Provider: q.Get("provider"), Currency: q.Get("currency"),
	}
	for _, s := range q["status"] {
		for _, part := range strings.Split(s, ",") {
			if st, ok := paymentStatusFromString(part); ok {
				req.Statuses = append(req.Statuses, st)
			}
		}
	}
	if v := q.Get("created_after"); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			httpx.WriteAppError(w, r, apperr.ErrParameterInvalid.WithMessage("created_after must be RFC 3339").WithParam("created_after"))
			return
		}
		req.CreatedAfter = timestamppb.New(t)
	}
	if v := q.Get("created_before"); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			httpx.WriteAppError(w, r, apperr.ErrParameterInvalid.WithMessage("created_before must be RFC 3339").WithParam("created_before"))
			return
		}
		req.CreatedBefore = timestamppb.New(t)
	}
	resp, err := g.payments.ListPayments(g.outgoing(r.Context(), r), req)
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	items := make([]*paymentJSON, 0, len(resp.GetPayments()))
	for _, p := range resp.GetPayments() {
		items = append(items, paymentToJSON(p, principal.LiveMode))
	}
	httpx.WriteJSON(w, http.StatusOK, listJSON[*paymentJSON]{Data: items, HasMore: resp.GetPage().GetHasMore(), NextCursor: nullable(resp.GetPage().GetNextPageToken())})
}

func (g *Gateway) capturePayment(w http.ResponseWriter, r *http.Request) {
	principal := PrincipalFromContext(r.Context())
	var in captureJSON
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	req := &paymentv1.CapturePaymentRequest{MerchantId: principal.MerchantID, PaymentId: chi.URLParam(r, "id"), IdempotencyKey: idempotencyKey(r), Amount: in.Amount.toProto()}
	if in.Final != nil {
		req.Final = in.Final
	}
	resp, err := g.payments.CapturePayment(g.outgoing(r.Context(), r), req)
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, paymentToJSON(resp.GetPayment(), principal.LiveMode))
}

func (g *Gateway) voidPayment(w http.ResponseWriter, r *http.Request) {
	principal := PrincipalFromContext(r.Context())
	var in voidJSON
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	switch in.Reason {
	case "", "requested_by_customer", "duplicate", "fraudulent", "abandoned":
	default:
		httpx.WriteAppError(w, r, apperr.ErrParameterInvalid.WithMessage("reason must be one of requested_by_customer|duplicate|fraudulent|abandoned").WithParam("reason"))
		return
	}
	resp, err := g.payments.VoidPayment(g.outgoing(r.Context(), r), &paymentv1.VoidPaymentRequest{MerchantId: principal.MerchantID, PaymentId: chi.URLParam(r, "id"), IdempotencyKey: idempotencyKey(r), Reason: in.Reason})
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, paymentToJSON(resp.GetPayment(), principal.LiveMode))
}

func (g *Gateway) confirmPayment(w http.ResponseWriter, r *http.Request) {
	principal := PrincipalFromContext(r.Context())
	var in confirmJSON
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	resp, err := g.payments.ConfirmPayment(g.outgoing(r.Context(), r), &paymentv1.ConfirmPaymentRequest{MerchantId: principal.MerchantID, PaymentId: chi.URLParam(r, "id"), IdempotencyKey: idempotencyKey(r), ProviderParams: in.ProviderParams})
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, paymentToJSON(resp.GetPayment(), principal.LiveMode))
}

func (g *Gateway) createRefund(w http.ResponseWriter, r *http.Request) {
	principal := PrincipalFromContext(r.Context())
	var in createRefundJSON
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	if in.PaymentID == "" {
		httpx.WriteAppError(w, r, apperr.ErrParameterMissing.WithMessage("payment_id is required").WithParam("payment_id"))
		return
	}
	resp, err := g.payments.CreateRefund(g.outgoing(r.Context(), r), &paymentv1.CreateRefundRequest{
		MerchantId: principal.MerchantID, PaymentId: in.PaymentID, IdempotencyKey: idempotencyKey(r),
		Amount: in.Amount.toProto(), Reason: in.Reason, Metadata: in.Metadata,
	})
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	if resp.GetIdempotentReplayed() {
		w.Header().Set(httpx.HeaderIdempotentReplayed, "true")
	}
	httpx.WriteJSON(w, http.StatusCreated, refundToJSON(resp.GetRefund(), principal.LiveMode))
}

func (g *Gateway) getRefund(w http.ResponseWriter, r *http.Request) {
	principal := PrincipalFromContext(r.Context())
	resp, err := g.payments.GetRefund(g.outgoing(r.Context(), r), &paymentv1.GetRefundRequest{MerchantId: principal.MerchantID, RefundId: chi.URLParam(r, "id")})
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, refundToJSON(resp.GetRefund(), principal.LiveMode))
}

// providerWebhook 轉交對應 adapter 驗簽與正規化；Phase 0 只記 log 並回 200。
// TODO(payment-service)：proto 尚無 IngestProviderWebhook rpc；新增後在此呼叫 payment-service 套用事件。
func (g *Gateway) providerWebhook(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "provider")
	client, ok := g.providers[name]
	if !ok {
		httpx.WriteAppError(w, r, apperr.ErrResourceMissing.WithMessage("unknown provider %q", name))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpx.WriteAppError(w, r, apperr.ErrParameterInvalid.WithMessage("unable to read body"))
		return
	}
	headers := map[string]*providerv1.HeaderValues{}
	for k, v := range r.Header {
		headers[k] = &providerv1.HeaderValues{Values: v}
	}
	resp, err := client.ParseWebhook(r.Context(), &providerv1.ParseWebhookRequest{Provider: name, Headers: headers, Body: body, ReceivedAt: timestamppb.Now(), RemoteIp: r.RemoteAddr})
	if err != nil {
		httpx.WriteAppError(w, r, err)
		return
	}
	if !resp.GetVerified() {
		httpx.WriteAppError(w, r, apperr.New(apperr.TypeInvalidRequest, "webhook_signature_invalid", "Webhook signature verification failed: "+resp.GetRejectionReason()))
		return
	}
	log := logx.FromContext(r.Context())
	if resp.GetIgnored() {
		log.Info("provider webhook ignored", "provider", name, "reason", resp.GetRejectionReason())
	} else {
		ev := resp.GetEvent()
		log.Info("provider webhook received (not yet applied; see TODO)", "provider", name,
			"event_type", ev.GetEventType().String(), "provider_event_id", ev.GetProviderEventId(), "payment_id", ev.GetPaymentId(), "provider_reference", ev.GetProviderReference())
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"received": true, "ignored": resp.GetIgnored()})
}

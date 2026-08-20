package app

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
)

// AggregateTypePayment 為 outbox.aggregate_type。
const AggregateTypePayment = "payment"

// buildOutboxMessage 把領域事件序列化成 pg.payment.v1.PaymentEvent（Kafka key = payment 公開 ID）。
func buildOutboxMessage(p *domain.Payment, ev domain.Event, traceID string) (outbox.Message, error) {
	eventID := ids.NewUUID().String()
	pe := &paymentv1.PaymentEvent{
		EventId:        eventID,
		EventType:      eventTypeToProto(ev.Type),
		OccurredAt:     timestamppb.New(ev.OccurredAt),
		MerchantId:     p.MerchantID,
		PaymentId:      p.PublicID,
		Livemode:       p.LiveMode,
		PaymentVersion: int64(ev.Seq),
		TraceId:        traceID,
		PaymentStatus:  StatusToProto(p.Status),
	}
	amount := p.Amount.ToProto()
	pay := ev.Payload
	switch ev.Type {
	case domain.EventPaymentCreated:
		pe.Payload = &paymentv1.PaymentEvent_PaymentCreated{PaymentCreated: &paymentv1.PaymentCreated{
			Amount: amount, CaptureMethod: CaptureMethodToProto(p.CaptureMethod), PaymentMethodType: MethodTypeToProto(p.PaymentMethodType),
			CustomerId: p.Customer.ID, Metadata: p.Metadata, Description: p.Description,
		}}
	case domain.EventPaymentRequiresAction:
		pe.Payload = &paymentv1.PaymentEvent_PaymentRequiresAction{PaymentRequiresAction: &paymentv1.PaymentRequiresAction{
			Amount: amount, Provider: p.SelectedProvider, ActionType: "redirect", ExpiresAt: tsOrNil(p.ExpiresAt),
		}}
	case domain.EventPaymentAuthorized:
		pe.Payload = &paymentv1.PaymentEvent_PaymentAuthorized{PaymentAuthorized: &paymentv1.PaymentAuthorized{
			Amount: amount, Provider: p.SelectedProvider, ProviderReference: p.ProviderReference,
			Fee:                    &commonv1.Money{AmountMinor: payInt(pay, "fee"), Currency: p.Amount.Currency},
			AuthorizationExpiresAt: tsOrNil(p.AuthExpiresAt), PaymentMethodDetails: DetailsToProto(p),
		}}
	case domain.EventPaymentCaptured:
		pe.Payload = &paymentv1.PaymentEvent_PaymentCaptured{PaymentCaptured: &paymentv1.PaymentCaptured{
			Amount:              &commonv1.Money{AmountMinor: payInt(pay, "amount"), Currency: p.Amount.Currency},
			TotalCapturedAmount: p.AmountCaptured.ToProto(), Provider: p.SelectedProvider, ProviderReference: p.ProviderReference,
			Fee: &commonv1.Money{AmountMinor: payInt(pay, "fee"), Currency: p.Amount.Currency}, IsFinal: true,
			PaymentMethodDetails: DetailsToProto(p), Metadata: p.Metadata,
		}}
	case domain.EventPaymentVoided:
		pe.Payload = &paymentv1.PaymentEvent_PaymentVoided{PaymentVoided: &paymentv1.PaymentVoided{
			Amount: p.AmountAuthorized.ToProto(), Provider: p.SelectedProvider, ProviderReference: p.ProviderReference, Reason: payStr(pay, "reason"),
		}}
	case domain.EventPaymentFailed:
		pf := &paymentv1.PaymentFailed{Amount: amount, Provider: p.SelectedProvider, AttemptCount: int32(len(p.Attempts))} //nolint:gosec // attempts ≤ 3
		if p.Failure != nil {
			pf.ErrorCategory = CategoryToProto(p.Failure.Category)
			pf.ErrorCode = p.Failure.PublicCode()
			pf.ErrorMessage = p.Failure.Message
		}
		pe.Payload = &paymentv1.PaymentEvent_PaymentFailed{PaymentFailed: pf}
	case domain.EventPaymentExpired:
		pe.Payload = &paymentv1.PaymentEvent_PaymentExpired{PaymentExpired: &paymentv1.PaymentExpired{
			Amount: amount, Provider: p.SelectedProvider, PreviousStatus: payStr(pay, "previous_status"),
		}}
	case domain.EventRefundCreated:
		pe.Payload = &paymentv1.PaymentEvent_RefundCreated{RefundCreated: &paymentv1.RefundCreated{
			RefundId: payStr(pay, "refund_id"), Amount: &commonv1.Money{AmountMinor: payInt(pay, "amount"), Currency: p.Amount.Currency},
			Provider: payStr(pay, "provider"), Reason: payStr(pay, "reason"),
		}}
	case domain.EventRefundSucceeded:
		pe.Payload = &paymentv1.PaymentEvent_RefundSucceeded{RefundSucceeded: &paymentv1.RefundSucceeded{
			RefundId: payStr(pay, "refund_id"), Amount: &commonv1.Money{AmountMinor: payInt(pay, "amount"), Currency: p.Amount.Currency},
			Provider: payStr(pay, "provider"), ProviderReference: payStr(pay, "provider_reference"),
		}}
	case domain.EventRefundFailed:
		pe.Payload = &paymentv1.PaymentEvent_RefundFailed{RefundFailed: &paymentv1.RefundFailed{
			RefundId: payStr(pay, "refund_id"), Amount: &commonv1.Money{AmountMinor: payInt(pay, "amount"), Currency: p.Amount.Currency},
			Provider: payStr(pay, "provider"), ErrorCode: payStr(pay, "error_code"), ErrorMessage: payStr(pay, "error_message"),
		}}
	}
	raw, err := proto.Marshal(pe)
	if err != nil {
		return outbox.Message{}, fmt.Errorf("marshal PaymentEvent: %w", err)
	}
	headers := map[string]string{"merchant_id": p.MerchantID, "schema_version": "1"}
	if traceID != "" {
		headers["trace_id"] = traceID
	}
	return outbox.Message{ID: eventID, AggregateType: AggregateTypePayment, AggregateID: p.PublicID, EventType: ev.Type, Payload: raw, Headers: headers}, nil
}

func eventTypeToProto(t string) paymentv1.PaymentEventType {
	switch t {
	case domain.EventPaymentCreated:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CREATED
	case domain.EventPaymentRequiresAction:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_REQUIRES_ACTION
	case domain.EventPaymentAuthorized:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_AUTHORIZED
	case domain.EventPaymentCaptured:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED
	case domain.EventPaymentVoided:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_VOIDED
	case domain.EventPaymentFailed:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_FAILED
	case domain.EventPaymentExpired:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_EXPIRED
	case domain.EventRefundCreated:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_CREATED
	case domain.EventRefundSucceeded:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_SUCCEEDED
	case domain.EventRefundFailed:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_FAILED
	default:
		return paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_UNSPECIFIED
	}
}

func payInt(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}

func payStr(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// StatusToProto 轉換 Payment 狀態。
func StatusToProto(s domain.Status) paymentv1.PaymentStatus {
	switch s {
	case domain.StatusCreated:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_CREATED
	case domain.StatusRequiresAction:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_REQUIRES_ACTION
	case domain.StatusAuthorized:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_AUTHORIZED
	case domain.StatusCaptured:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_CAPTURED
	case domain.StatusVoided:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_VOIDED
	case domain.StatusPartiallyRefunded:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_PARTIALLY_REFUNDED
	case domain.StatusRefunded:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_REFUNDED
	case domain.StatusDisputed:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_DISPUTED
	case domain.StatusChargebackWon:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_CHARGEBACK_WON
	case domain.StatusChargebackLost:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_CHARGEBACK_LOST
	case domain.StatusFailed:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_FAILED
	case domain.StatusExpired:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_EXPIRED
	default:
		return paymentv1.PaymentStatus_PAYMENT_STATUS_UNSPECIFIED
	}
}

// StatusFromProto 轉回領域狀態（UNSPECIFIED 回空字串）。
func StatusFromProto(s paymentv1.PaymentStatus) domain.Status {
	for _, d := range domain.AllStatuses {
		if StatusToProto(d) == s {
			return d
		}
	}
	return ""
}

// CaptureMethodToProto 轉換請款方式。
func CaptureMethodToProto(c domain.CaptureMethod) paymentv1.CaptureMethod {
	if c == domain.CaptureManual {
		return paymentv1.CaptureMethod_CAPTURE_METHOD_MANUAL
	}
	return paymentv1.CaptureMethod_CAPTURE_METHOD_AUTOMATIC
}

// MethodTypeToProto 轉換付款方式大類。
func MethodTypeToProto(t string) paymentv1.PaymentMethodType {
	switch t {
	case "card":
		return paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_CARD
	case "wallet":
		return paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_WALLET
	case "bank_transfer":
		return paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_BANK_TRANSFER
	default:
		return paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_UNSPECIFIED
	}
}

// DetailsToProto 轉換付款工具非敏感資訊。
func DetailsToProto(p *domain.Payment) *paymentv1.PaymentMethodDetails {
	d := p.PaymentMethodDetails
	out := &paymentv1.PaymentMethodDetails{
		Type: MethodTypeToProto(p.PaymentMethodType), Brand: d.Brand, Last4: d.Last4, Issuer: d.Issuer,
		IssuerCountry: d.IssuerCountry, Funding: d.Funding, ThreeDsResult: d.ThreeDSResult,
	}
	if d.WalletType != "" {
		out.Brand = d.WalletType
	}
	if d.BankCode != "" {
		out.Brand = d.BankCode
	}
	return out
}

func tsOrNil(t *time.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(*t)
}

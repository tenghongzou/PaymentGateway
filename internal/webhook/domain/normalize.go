package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
)

// FromPaymentEvent 把 Kafka payment.events 的 PaymentEvent 正規化成對商戶的 Event：
// 事件名稱由 enum 推導、payload 轉成 OpenAPI Event JSON（data.object 為 payment / refund / dispute 快照）。
// now 用於 occurred_at 缺失時的後備值。
func FromPaymentEvent(pe *paymentv1.PaymentEvent, now time.Time) (*Event, error) {
	if pe == nil {
		return nil, fmt.Errorf("%w: nil event", ErrUnsupportedEvent)
	}
	typeName := EventTypeFromEnumName(paymentv1.PaymentEventType_name[int32(pe.GetEventType())])
	if !IsKnownEventType(typeName) {
		return nil, fmt.Errorf("%w: event_type=%s", ErrUnsupportedEvent, pe.GetEventType())
	}
	id, err := ParseEventID(pe.GetEventId())
	if err != nil {
		return nil, err
	}
	merchantID, err := ParseMerchantID(pe.GetMerchantId())
	if err != nil {
		return nil, err
	}
	occurred := tsOrZero(pe.GetOccurredAt())
	if occurred.IsZero() {
		occurred = now
	}
	occurred = occurred.UTC()

	ev := &Event{
		ID:         id,
		MerchantID: merchantID,
		Type:       typeName,
		PaymentID:  pe.GetPaymentId(),
		Livemode:   pe.GetLivemode(),
		OccurredAt: occurred,
		CreatedAt:  now.UTC(),
	}
	obj, resourceType, resourceID, err := buildObject(pe, typeName, occurred)
	if err != nil {
		return nil, err
	}
	ev.ResourceType = resourceType
	ev.ResourceID = resourceID

	body := EventJSON{
		ID:         EventPublicID(id),
		Object:     "event",
		Type:       typeName,
		APIVersion: APIVersion,
		Livemode:   pe.GetLivemode(),
		CreatedAt:  occurred,
		PaymentID:  pe.GetPaymentId(),
		Request:    nil, // 來源事件未攜帶觸發請求資訊；TODO: events.proto 補 request_id 後填入
		Data:       EventDataJSON{Object: obj},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("webhook: marshal event payload: %w", err)
	}
	ev.Payload = payload
	return ev, nil
}

// buildObject 依 oneof payload 組出 data.object，回傳 (物件, resource_type, resource_id)。
func buildObject(pe *paymentv1.PaymentEvent, typeName string, occurred time.Time) (any, string, string, error) {
	paymentID := pe.GetPaymentId()
	livemode := pe.GetLivemode()
	status := paymentStatusString(pe.GetPaymentStatus(), typeName)
	base := PaymentJSON{
		ID: paymentID, Object: ResourcePayment, Status: status, Version: pe.GetPaymentVersion(),
		Livemode: livemode, UpdatedAt: occurred, Metadata: map[string]string{},
	}

	switch p := pe.GetPayload().(type) {
	case *paymentv1.PaymentEvent_PaymentCreated:
		base.Amount = moneyJSON(p.PaymentCreated.GetAmount())
		base.CaptureMethod = enumLower(paymentv1.CaptureMethod_name[int32(p.PaymentCreated.GetCaptureMethod())], "CAPTURE_METHOD_")
		if t := enumLower(paymentv1.PaymentMethodType_name[int32(p.PaymentCreated.GetPaymentMethodType())], "PAYMENT_METHOD_TYPE_"); t != "" {
			base.PaymentMethod = &PaymentMethodJSON{Type: t}
		}
		if c := p.PaymentCreated.GetCustomerId(); c != "" {
			base.Customer = &CustomerJSON{ID: c}
		}
		base.Metadata = nonNilMap(p.PaymentCreated.GetMetadata())
		base.Description = p.PaymentCreated.GetDescription()
		return base, ResourcePayment, paymentID, nil

	case *paymentv1.PaymentEvent_PaymentRequiresAction:
		base.Amount = moneyJSON(p.PaymentRequiresAction.GetAmount())
		base.Provider = p.PaymentRequiresAction.GetProvider()
		base.NextAction = &NextActionJSON{Type: p.PaymentRequiresAction.GetActionType(), ExpiresAt: tsPtr(p.PaymentRequiresAction.GetExpiresAt())}
		return base, ResourcePayment, paymentID, nil

	case *paymentv1.PaymentEvent_PaymentAuthorized:
		base.Amount = moneyJSON(p.PaymentAuthorized.GetAmount())
		base.Provider = p.PaymentAuthorized.GetProvider()
		base.ProviderReference = p.PaymentAuthorized.GetProviderReference()
		base.Fee = moneyJSON(p.PaymentAuthorized.GetFee())
		base.AuthorizationExpiresAt = tsPtr(p.PaymentAuthorized.GetAuthorizationExpiresAt())
		base.PaymentMethod = paymentMethodJSON(p.PaymentAuthorized.GetPaymentMethodDetails())
		return base, ResourcePayment, paymentID, nil

	case *paymentv1.PaymentEvent_PaymentCaptured:
		base.Amount = moneyJSON(p.PaymentCaptured.GetAmount())
		base.CapturedAmount = moneyJSON(p.PaymentCaptured.GetTotalCapturedAmount())
		if base.CapturedAmount == nil {
			base.CapturedAmount = base.Amount
		}
		if base.Amount != nil {
			base.RefundedAmount = &MoneyJSON{AmountMinor: 0, Currency: base.Amount.Currency}
		}
		base.Provider = p.PaymentCaptured.GetProvider()
		base.ProviderReference = p.PaymentCaptured.GetProviderReference()
		base.Fee = moneyJSON(p.PaymentCaptured.GetFee())
		isFinal := p.PaymentCaptured.GetIsFinal()
		base.IsFinalCapture = &isFinal
		base.PaymentMethod = paymentMethodJSON(p.PaymentCaptured.GetPaymentMethodDetails())
		base.Metadata = nonNilMap(p.PaymentCaptured.GetMetadata())
		return base, ResourcePayment, paymentID, nil

	case *paymentv1.PaymentEvent_PaymentVoided:
		base.Amount = moneyJSON(p.PaymentVoided.GetAmount())
		base.Provider = p.PaymentVoided.GetProvider()
		base.ProviderReference = p.PaymentVoided.GetProviderReference()
		base.VoidReason = p.PaymentVoided.GetReason()
		return base, ResourcePayment, paymentID, nil

	case *paymentv1.PaymentEvent_PaymentFailed:
		base.Amount = moneyJSON(p.PaymentFailed.GetAmount())
		base.Provider = p.PaymentFailed.GetProvider()
		base.LastError = &ErrorDetailJSON{
			Type:    errorTypeForCategory(p.PaymentFailed.GetErrorCategory()),
			Code:    p.PaymentFailed.GetErrorCode(),
			Message: p.PaymentFailed.GetErrorMessage(),
		}
		return base, ResourcePayment, paymentID, nil

	case *paymentv1.PaymentEvent_PaymentExpired:
		base.Amount = moneyJSON(p.PaymentExpired.GetAmount())
		base.Provider = p.PaymentExpired.GetProvider()
		base.PreviousStatus = p.PaymentExpired.GetPreviousStatus()
		return base, ResourcePayment, paymentID, nil

	case *paymentv1.PaymentEvent_RefundCreated:
		r := RefundJSON{
			ID: p.RefundCreated.GetRefundId(), Object: ResourceRefund, PaymentID: paymentID, Status: "pending",
			Amount: moneyJSON(p.RefundCreated.GetAmount()), Provider: p.RefundCreated.GetProvider(),
			Reason: p.RefundCreated.GetReason(), Metadata: nonNilMap(p.RefundCreated.GetMetadata()),
			Livemode: livemode, UpdatedAt: occurred,
		}
		return r, ResourceRefund, r.ID, nil

	case *paymentv1.PaymentEvent_RefundSucceeded:
		done := occurred
		r := RefundJSON{
			ID: p.RefundSucceeded.GetRefundId(), Object: ResourceRefund, PaymentID: paymentID, Status: "succeeded",
			Amount: moneyJSON(p.RefundSucceeded.GetAmount()), Provider: p.RefundSucceeded.GetProvider(),
			ProviderReference: p.RefundSucceeded.GetProviderReference(), Fee: moneyJSON(p.RefundSucceeded.GetFee()),
			Metadata: map[string]string{}, Livemode: livemode, UpdatedAt: occurred, CompletedAt: &done,
		}
		return r, ResourceRefund, r.ID, nil

	case *paymentv1.PaymentEvent_RefundFailed:
		done := occurred
		r := RefundJSON{
			ID: p.RefundFailed.GetRefundId(), Object: ResourceRefund, PaymentID: paymentID, Status: "failed",
			Amount: moneyJSON(p.RefundFailed.GetAmount()), Provider: p.RefundFailed.GetProvider(),
			FailureCode: p.RefundFailed.GetErrorCode(), FailureMessage: p.RefundFailed.GetErrorMessage(),
			Metadata: map[string]string{}, Livemode: livemode, UpdatedAt: occurred, CompletedAt: &done,
		}
		return r, ResourceRefund, r.ID, nil

	case *paymentv1.PaymentEvent_DisputeOpened:
		d := DisputeJSON{
			ID: p.DisputeOpened.GetDisputeId(), Object: ResourceDispute, PaymentID: paymentID, Status: "needs_response",
			Amount: moneyJSON(p.DisputeOpened.GetAmount()), Fee: moneyJSON(p.DisputeOpened.GetFee()),
			Reason: p.DisputeOpened.GetReason(), Provider: p.DisputeOpened.GetProvider(),
			ProviderReference: p.DisputeOpened.GetProviderReference(), EvidenceDueBy: tsPtr(p.DisputeOpened.GetEvidenceDueBy()),
			Livemode: livemode, UpdatedAt: occurred,
		}
		return d, ResourceDispute, d.ID, nil

	case *paymentv1.PaymentEvent_DisputeEvidenceSubmitted:
		d := DisputeJSON{
			ID: p.DisputeEvidenceSubmitted.GetDisputeId(), Object: ResourceDispute, PaymentID: paymentID, Status: "under_review",
			Provider: p.DisputeEvidenceSubmitted.GetProvider(), EvidenceSubmittedAt: tsPtr(p.DisputeEvidenceSubmitted.GetSubmittedAt()),
			Livemode: livemode, UpdatedAt: occurred,
		}
		return d, ResourceDispute, d.ID, nil

	case *paymentv1.PaymentEvent_DisputeClosed:
		status := "won"
		if p.DisputeClosed.GetOutcome() == providerv1.DisputeOutcome_DISPUTE_OUTCOME_LOST || typeName == "dispute.lost" {
			status = "lost"
		}
		d := DisputeJSON{
			ID: p.DisputeClosed.GetDisputeId(), Object: ResourceDispute, PaymentID: paymentID, Status: status,
			Amount: moneyJSON(p.DisputeClosed.GetAmount()), Fee: moneyJSON(p.DisputeClosed.GetFee()),
			Provider: p.DisputeClosed.GetProvider(), ProviderReference: p.DisputeClosed.GetProviderReference(),
			ClosedAt: tsPtr(p.DisputeClosed.GetClosedAt()), Livemode: livemode, UpdatedAt: occurred,
		}
		return d, ResourceDispute, d.ID, nil

	default:
		return nil, "", "", fmt.Errorf("%w: event %s has no payload", ErrUnsupportedEvent, typeName)
	}
}

// paymentStatusString 把 PaymentStatus enum 轉小寫字串；未指定時依事件類型推導。
func paymentStatusString(s paymentv1.PaymentStatus, typeName string) string {
	if s != paymentv1.PaymentStatus_PAYMENT_STATUS_UNSPECIFIED {
		return enumLower(paymentv1.PaymentStatus_name[int32(s)], "PAYMENT_STATUS_")
	}
	switch typeName {
	case "payment.created":
		return "created"
	case "payment.requires_action":
		return "requires_action"
	case "payment.authorized":
		return "authorized"
	case "payment.captured":
		return "captured"
	case "payment.voided":
		return "voided"
	case "payment.failed":
		return "failed"
	case "payment.expired":
		return "expired"
	case "dispute.opened", "dispute.evidence_submitted":
		return "disputed"
	case "dispute.won":
		return "chargeback_won"
	case "dispute.lost":
		return "chargeback_lost"
	default:
		return "captured"
	}
}

// errorTypeForCategory 對應 docs/02 §10：PSP 拒絕 → provider_error；我方參數問題 → invalid_request_error。
func errorTypeForCategory(c providerv1.ProviderErrorCategory) string {
	switch c {
	case providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST:
		return "invalid_request_error"
	case providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_UNSPECIFIED, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_UNKNOWN:
		return "api_error"
	default:
		return "provider_error"
	}
}

func enumLower(name, prefix string) string {
	s := strings.ToLower(strings.TrimPrefix(name, prefix))
	if s == "unspecified" {
		return ""
	}
	return s
}

func moneyJSON(m *commonv1.Money) *MoneyJSON {
	if m == nil || (m.GetCurrency() == "" && m.GetAmountMinor() == 0) {
		return nil
	}
	return &MoneyJSON{AmountMinor: m.GetAmountMinor(), Currency: m.GetCurrency()}
}

func paymentMethodJSON(d *paymentv1.PaymentMethodDetails) *PaymentMethodJSON {
	if d == nil {
		return nil
	}
	return &PaymentMethodJSON{
		Type:  enumLower(paymentv1.PaymentMethodType_name[int32(d.GetType())], "PAYMENT_METHOD_TYPE_"),
		Brand: d.GetBrand(), Last4: d.GetLast4(), Issuer: d.GetIssuer(), IssuerCountry: d.GetIssuerCountry(),
		Funding: d.GetFunding(), ThreeDSResult: d.GetThreeDsResult(),
	}
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func tsOrZero(ts *timestamppb.Timestamp) time.Time {
	if ts == nil || !ts.IsValid() {
		return time.Time{}
	}
	return ts.AsTime()
}

func tsPtr(ts *timestamppb.Timestamp) *time.Time {
	t := tsOrZero(ts)
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

// NewEventID 產生測試 / 開發用的事件 uuid（正式流程一律沿用來源 event_id）。
func NewEventID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

func TestEventTypeFromEnumName(t *testing.T) {
	for v, name := range paymentv1.PaymentEventType_name {
		got := EventTypeFromEnumName(name)
		if v == 0 {
			assert.Empty(t, got)
			continue
		}
		assert.True(t, IsKnownEventType(got), "%s → %q must be a known event type", name, got)
	}
	assert.Equal(t, "payment.requires_action", EventTypeFromEnumName("PAYMENT_EVENT_TYPE_PAYMENT_REQUIRES_ACTION"))
	assert.Equal(t, "dispute.evidence_submitted", EventTypeFromEnumName("PAYMENT_EVENT_TYPE_DISPUTE_EVIDENCE_SUBMITTED"))
	assert.Len(t, EventTypes, 14)
}

func TestFromPaymentEvent_Captured(t *testing.T) {
	now := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	evUUID := ids.NewUUID()
	mchUUID := ids.NewUUID()
	pe := &paymentv1.PaymentEvent{
		EventId:        ids.Format(ids.PrefixEvent, evUUID),
		EventType:      paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED,
		OccurredAt:     timestamppb.New(now.Add(-time.Second)),
		MerchantId:     ids.Format(ids.PrefixMerchant, mchUUID),
		PaymentId:      "pay_01J5X9Q3K8T2M4N6P8R0S2T4V6",
		Livemode:       true,
		PaymentVersion: 7,
		PaymentStatus:  paymentv1.PaymentStatus_PAYMENT_STATUS_CAPTURED,
		Payload: &paymentv1.PaymentEvent_PaymentCaptured{PaymentCaptured: &paymentv1.PaymentCaptured{
			Amount:              &commonv1.Money{AmountMinor: 150000, Currency: "TWD"},
			TotalCapturedAmount: &commonv1.Money{AmountMinor: 150000, Currency: "TWD"},
			Provider:            "stripe",
			ProviderReference:   "pi_3NXyzABC",
			Fee:                 &commonv1.Money{AmountMinor: 4380, Currency: "TWD"},
			IsFinal:             true,
			PaymentMethodDetails: &paymentv1.PaymentMethodDetails{
				Type: paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_CARD, Brand: "visa", Last4: "4242",
			},
			Metadata: map[string]string{"order_id": "A10023"},
		}},
	}
	ev, err := FromPaymentEvent(pe, now)
	require.NoError(t, err)
	assert.Equal(t, evUUID, ev.ID)
	assert.Equal(t, mchUUID, ev.MerchantID)
	assert.Equal(t, "payment.captured", ev.Type)
	assert.Equal(t, ResourcePayment, ev.ResourceType)
	assert.Equal(t, "pay_01J5X9Q3K8T2M4N6P8R0S2T4V6", ev.ResourceID)
	assert.Equal(t, now.Add(-time.Second), ev.OccurredAt)
	assert.True(t, ev.Livemode)

	var got map[string]any
	require.NoError(t, json.Unmarshal(ev.Payload, &got))
	assert.Equal(t, ids.Format(ids.PrefixEvent, evUUID), got["id"])
	assert.Equal(t, "event", got["object"])
	assert.Equal(t, "payment.captured", got["type"])
	assert.Equal(t, "v1", got["api_version"])
	assert.Equal(t, true, got["livemode"])
	assert.Equal(t, "2026-08-20T07:59:59Z", got["created_at"])
	assert.Equal(t, "pay_01J5X9Q3K8T2M4N6P8R0S2T4V6", got["payment_id"])
	assert.Contains(t, got, "request")
	assert.Nil(t, got["request"])

	data, ok := got["data"].(map[string]any)
	require.True(t, ok)
	obj, ok := data["object"].(map[string]any)
	require.True(t, ok)
	// encoding/json 解回 map[string]any 的數字為浮點，金額欄位以 EqualValues 斷言避免直接寫浮點字面值。
	assertMoney := func(v any, minor int64, currency string) {
		t.Helper()
		m, mok := v.(map[string]any)
		require.True(t, mok)
		assert.EqualValues(t, minor, m["amount_minor"])
		assert.Equal(t, currency, m["currency"])
	}
	assert.Equal(t, "payment", obj["object"])
	assert.Equal(t, "pay_01J5X9Q3K8T2M4N6P8R0S2T4V6", obj["id"])
	assert.Equal(t, "captured", obj["status"])
	assert.EqualValues(t, 7, obj["version"])
	assertMoney(obj["amount"], 150000, "TWD")
	assertMoney(obj["captured_amount"], 150000, "TWD")
	assertMoney(obj["refunded_amount"], 0, "TWD")
	assert.Equal(t, "stripe", obj["provider"])
	assert.Equal(t, "pi_3NXyzABC", obj["provider_reference"])
	assert.Equal(t, map[string]any{"order_id": "A10023"}, obj["metadata"])
	pm, ok := obj["payment_method"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "card", pm["type"])
	assert.Equal(t, "4242", pm["last4"])
	assert.Equal(t, true, obj["is_final_capture"])
}

func TestFromPaymentEvent_RefundAndDispute(t *testing.T) {
	now := time.Now().UTC()
	base := func(typ paymentv1.PaymentEventType) *paymentv1.PaymentEvent {
		return &paymentv1.PaymentEvent{
			EventId: ids.New(ids.PrefixEvent), EventType: typ, MerchantId: ids.New(ids.PrefixMerchant),
			PaymentId: "pay_x", Livemode: false, PaymentStatus: paymentv1.PaymentStatus_PAYMENT_STATUS_CAPTURED,
		}
	}
	pe := base(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_SUCCEEDED)
	pe.Payload = &paymentv1.PaymentEvent_RefundSucceeded{RefundSucceeded: &paymentv1.RefundSucceeded{
		RefundId: "re_1", Amount: &commonv1.Money{AmountMinor: 500, Currency: "TWD"}, Provider: "mock", ProviderReference: "rf_1",
	}}
	ev, err := FromPaymentEvent(pe, now)
	require.NoError(t, err)
	assert.Equal(t, ResourceRefund, ev.ResourceType)
	assert.Equal(t, "re_1", ev.ResourceID)
	assert.Equal(t, "pay_x", ev.PaymentID)
	// occurred_at 缺失時以 now 補上。
	assert.Equal(t, now, ev.OccurredAt)
	var got struct {
		Data struct {
			Object RefundJSON `json:"object"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(ev.Payload, &got))
	assert.Equal(t, "refund", got.Data.Object.Object)
	assert.Equal(t, "succeeded", got.Data.Object.Status)
	assert.Equal(t, "pay_x", got.Data.Object.PaymentID)
	require.NotNil(t, got.Data.Object.CompletedAt)

	pe = base(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_LOST)
	pe.Payload = &paymentv1.PaymentEvent_DisputeClosed{DisputeClosed: &paymentv1.DisputeClosed{
		DisputeId: "dp_1", Amount: &commonv1.Money{AmountMinor: 500, Currency: "TWD"},
		Outcome: providerv1.DisputeOutcome_DISPUTE_OUTCOME_LOST, ClosedAt: timestamppb.New(now),
	}}
	ev, err = FromPaymentEvent(pe, now)
	require.NoError(t, err)
	assert.Equal(t, "dispute.lost", ev.Type)
	assert.Equal(t, ResourceDispute, ev.ResourceType)
	assert.Equal(t, "dp_1", ev.ResourceID)
	var gotD struct {
		Data struct {
			Object DisputeJSON `json:"object"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(ev.Payload, &gotD))
	assert.Equal(t, "lost", gotD.Data.Object.Status)

	// payment.failed 的 last_error。
	pe = base(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_FAILED)
	pe.PaymentStatus = paymentv1.PaymentStatus_PAYMENT_STATUS_FAILED
	pe.Payload = &paymentv1.PaymentEvent_PaymentFailed{PaymentFailed: &paymentv1.PaymentFailed{
		Amount: &commonv1.Money{AmountMinor: 100, Currency: "USD"}, Provider: "stripe",
		ErrorCategory: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_SOFT, ErrorCode: "card_declined", ErrorMessage: "Insufficient funds",
	}}
	ev, err = FromPaymentEvent(pe, now)
	require.NoError(t, err)
	var gotP struct {
		Data struct {
			Object PaymentJSON `json:"object"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(ev.Payload, &gotP))
	assert.Equal(t, "failed", gotP.Data.Object.Status)
	require.NotNil(t, gotP.Data.Object.LastError)
	assert.Equal(t, "provider_error", gotP.Data.Object.LastError.Type)
	assert.Equal(t, "card_declined", gotP.Data.Object.LastError.Code)
}

func TestFromPaymentEvent_Errors(t *testing.T) {
	now := time.Now()
	_, err := FromPaymentEvent(nil, now)
	require.ErrorIs(t, err, ErrUnsupportedEvent)
	_, err = FromPaymentEvent(&paymentv1.PaymentEvent{EventId: ids.New(ids.PrefixEvent), MerchantId: ids.New(ids.PrefixMerchant)}, now)
	require.ErrorIs(t, err, ErrUnsupportedEvent, "unspecified type")
	_, err = FromPaymentEvent(&paymentv1.PaymentEvent{
		EventId: "bogus", MerchantId: ids.New(ids.PrefixMerchant), EventType: paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_VOIDED,
	}, now)
	require.ErrorIs(t, err, ErrInvalidID)
	// 缺 payload。
	_, err = FromPaymentEvent(&paymentv1.PaymentEvent{
		EventId: ids.New(ids.PrefixEvent), MerchantId: ids.New(ids.PrefixMerchant), EventType: paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_VOIDED,
	}, now)
	require.ErrorIs(t, err, ErrUnsupportedEvent)
	// 純 uuid 也接受。
	u := uuid.New()
	got, err := ParseEventID(u.String())
	require.NoError(t, err)
	assert.Equal(t, u, got)
	assert.Equal(t, "evt_"+ids.Format(ids.PrefixEvent, u)[4:], EventPublicID(u))
}

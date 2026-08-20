package app

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

func pmoney(n int64) *commonv1.Money { return &commonv1.Money{AmountMinor: n, Currency: "TWD"} }

// envelope 建立帶 payload 的 PaymentEvent 與 Kafka record。
func envelope(t *testing.T, typ paymentv1.PaymentEventType, merchant uuid.UUID, payload any) (eventbus.Record, string) {
	t.Helper()
	eventID := ids.New(ids.PrefixEvent)
	ev := &paymentv1.PaymentEvent{
		EventId:    eventID,
		EventType:  typ,
		OccurredAt: timestamppb.New(time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)),
		MerchantId: ids.Format(ids.PrefixMerchant, merchant),
		PaymentId:  "pay_1",
		Livemode:   true,
	}
	switch p := payload.(type) {
	case *paymentv1.PaymentCaptured:
		ev.Payload = &paymentv1.PaymentEvent_PaymentCaptured{PaymentCaptured: p}
	case *paymentv1.PaymentAuthorized:
		ev.Payload = &paymentv1.PaymentEvent_PaymentAuthorized{PaymentAuthorized: p}
	case *paymentv1.RefundCreated:
		ev.Payload = &paymentv1.PaymentEvent_RefundCreated{RefundCreated: p}
	case *paymentv1.RefundSucceeded:
		ev.Payload = &paymentv1.PaymentEvent_RefundSucceeded{RefundSucceeded: p}
	case *paymentv1.RefundFailed:
		ev.Payload = &paymentv1.PaymentEvent_RefundFailed{RefundFailed: p}
	case *paymentv1.DisputeOpened:
		ev.Payload = &paymentv1.PaymentEvent_DisputeOpened{DisputeOpened: p}
	case *paymentv1.DisputeClosed:
		ev.Payload = &paymentv1.PaymentEvent_DisputeClosed{DisputeClosed: p}
	case nil:
	default:
		t.Fatalf("unsupported payload %T", payload)
	}
	raw, err := proto.Marshal(ev)
	require.NoError(t, err)
	return eventbus.Record{
		Topic: eventbus.TopicPaymentEvents, Key: "pay_1", Value: raw,
		Headers: map[string]string{eventbus.HeaderEventID: eventID, eventbus.HeaderEventType: "payment.captured"},
	}, eventID
}

func TestHandlePaymentEvent_CapturePostsJCAP(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	ctx := context.Background()
	rec, eventID := envelope(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, merchantA,
		&paymentv1.PaymentCaptured{Amount: pmoney(1000), Fee: pmoney(33), Provider: "stripe"})

	require.NoError(t, svc.HandlePaymentEvent(ctx, rec))
	require.Len(t, f.journals, 1)
	j := f.journals[0]
	assert.Equal(t, domain.TemplateJCAP, j.Template)
	assert.Equal(t, eventID, j.SourceID)
	u, _ := ids.ParseWithPrefix(eventID, ids.PrefixEvent)
	assert.Equal(t, u, j.EventID)
	assert.Equal(t, "pay_1", j.Metadata[domain.MetaPaymentID])
	assert.Equal(t, int64(967), f.balances.Of(domain.MerchantPayable(merchantA, "TWD", true)))
	assert.Len(t, f.outbox, 1)
	assert.True(t, f.processed[u.String()+"|"+ConsumerPaymentEvents])

	// 重送同一訊息：去重，無新 journal / outbox
	require.NoError(t, svc.HandlePaymentEvent(ctx, rec))
	assert.Len(t, f.journals, 1)
	assert.Len(t, f.outbox, 1)
}

func TestHandlePaymentEvent_NoTemplateIsAcked(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	rec, eventID := envelope(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_AUTHORIZED, merchantA,
		&paymentv1.PaymentAuthorized{Amount: pmoney(1000), Provider: "stripe"})
	require.NoError(t, svc.HandlePaymentEvent(context.Background(), rec))
	assert.Empty(t, f.journals)
	assert.Empty(t, f.outbox)
	u, _ := ids.ParseWithPrefix(eventID, ids.PrefixEvent)
	assert.True(t, f.processed[u.String()+"|"+ConsumerPaymentEvents], "processed_events still records the event")
}

func TestHandlePaymentEvent_PoisonMessages(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	ctx := context.Background()

	// 無法反序列化
	err := svc.HandlePaymentEvent(ctx, eventbus.Record{Value: []byte{0xff, 0xfe, 0x01}, Headers: map[string]string{eventbus.HeaderEventID: ids.New(ids.PrefixEvent)}})
	assert.ErrorIs(t, err, ErrPoisonMessage)

	// event_id 格式錯誤
	rec, _ := envelope(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, merchantA,
		&paymentv1.PaymentCaptured{Amount: pmoney(1000), Provider: "stripe"})
	rec.Headers[eventbus.HeaderEventID] = "garbage"
	assert.ErrorIs(t, svc.HandlePaymentEvent(ctx, rec), ErrPoisonMessage)

	// 缺 provider → 範本無法建立 → poison，且 processed_events 已 rollback（重試時會再處理）
	rec, eventID := envelope(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, merchantA,
		&paymentv1.PaymentCaptured{Amount: pmoney(1000)})
	assert.ErrorIs(t, svc.HandlePaymentEvent(ctx, rec), ErrPoisonMessage)
	u, _ := ids.ParseWithPrefix(eventID, ids.PrefixEvent)
	assert.False(t, f.processed[u.String()+"|"+ConsumerPaymentEvents])
	assert.Empty(t, f.journals)

	// fee > amount
	rec, _ = envelope(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, merchantA,
		&paymentv1.PaymentCaptured{Amount: pmoney(10), Fee: pmoney(11), Provider: "stripe"})
	assert.ErrorIs(t, svc.HandlePaymentEvent(ctx, rec), ErrPoisonMessage)

	// 壞的 merchant_id
	rec, _ = envelope(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, merchantA,
		&paymentv1.PaymentCaptured{Amount: pmoney(10), Provider: "stripe"})
	var ev paymentv1.PaymentEvent
	require.NoError(t, proto.Unmarshal(rec.Value, &ev))
	ev.MerchantId = "mch_bad"
	rec.Value, _ = proto.Marshal(&ev)
	assert.ErrorIs(t, svc.HandlePaymentEvent(ctx, rec), ErrPoisonMessage)
}

func TestHandlePaymentEvent_EventIDFallsBackToPayload(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	rec, eventID := envelope(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, merchantA,
		&paymentv1.PaymentCaptured{Amount: pmoney(1000), Provider: "mock"})
	delete(rec.Headers, eventbus.HeaderEventID)
	require.NoError(t, svc.HandlePaymentEvent(context.Background(), rec))
	require.Len(t, f.journals, 1)
	u, _ := ids.ParseWithPrefix(eventID, ids.PrefixEvent)
	assert.Equal(t, u, f.journals[0].EventID)
}

// TestHandlePaymentEvent_FullLifecycle 跑完 捕獲 → 退款 pending/成功 → 退款失敗 → 爭議開啟 → 敗訴，檢查每步餘額與 reversal 關聯。
func TestHandlePaymentEvent_FullLifecycle(t *testing.T) {
	svc, f := newTestService(domain.Policy{RefundChargebackFeeOnWin: true})
	ctx := context.Background()
	m := merchantA
	handle := func(typ paymentv1.PaymentEventType, payload any) {
		t.Helper()
		rec, _ := envelope(t, typ, m, payload)
		require.NoError(t, svc.HandlePaymentEvent(ctx, rec))
	}
	payable := func() int64 { return f.balances.Of(domain.MerchantPayable(m, "TWD", true)) }

	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, &paymentv1.PaymentCaptured{Amount: pmoney(1000), Fee: pmoney(33), Provider: "stripe"})
	assert.Equal(t, int64(967), payable())

	// 退款 300：pending → succeeded（含 5 元退款費）
	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_CREATED, &paymentv1.RefundCreated{RefundId: "re_1", Amount: pmoney(300), Provider: "stripe"})
	assert.Equal(t, int64(667), payable())
	assert.Equal(t, int64(300), f.balances.Of(domain.RefundClearing(m, "TWD", true)))
	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_SUCCEEDED, &paymentv1.RefundSucceeded{RefundId: "re_1", Amount: pmoney(300), Provider: "stripe", Fee: pmoney(5)})
	assert.Equal(t, int64(662), payable())
	assert.Zero(t, f.balances.Of(domain.RefundClearing(m, "TWD", true)))
	assert.Equal(t, int64(700), f.balances.Of(domain.PSPReceivable("stripe", "TWD", true)))
	assert.Equal(t, int64(38), f.balances.Of(domain.FeeRevenue("TWD", true)))

	// 退款 100：pending → failed（J-REF-FAIL 以 reversal_of 指向 J-REF-PEND）
	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_CREATED, &paymentv1.RefundCreated{RefundId: "re_2", Amount: pmoney(100), Provider: "stripe"})
	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_FAILED, &paymentv1.RefundFailed{RefundId: "re_2", Amount: pmoney(100), Provider: "stripe"})
	assert.Equal(t, int64(662), payable())
	var pend, fail *domain.Journal
	for _, j := range f.journals {
		switch {
		case j.Template == domain.TemplateJREFPEND && j.ReferenceID == "re_2":
			pend = j
		case j.Template == domain.TemplateJREFFail:
			fail = j
		}
	}
	require.NotNil(t, pend)
	require.NotNil(t, fail)
	require.NotNil(t, fail.ReversalOf)
	assert.Equal(t, pend.ID, *fail.ReversalOf)

	// 爭議 600 + 拒付費 450 → 敗訴
	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_OPENED, &paymentv1.DisputeOpened{DisputeId: "dp_1", Amount: pmoney(600), Fee: pmoney(450), Provider: "stripe"})
	assert.Equal(t, int64(662-600-450), payable())
	assert.Equal(t, int64(600), f.balances.Of(domain.ChargebackReserve(m, "TWD", true)))
	assert.Equal(t, int64(450), f.balances.Of(domain.ChargebackFeeRevenue("TWD", true)))
	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_LOST, &paymentv1.DisputeClosed{DisputeId: "dp_1", Amount: pmoney(600), Fee: pmoney(450), Provider: "stripe", Outcome: providerv1.DisputeOutcome_DISPUTE_OUTCOME_LOST})
	assert.Zero(t, f.balances.Of(domain.ChargebackReserve(m, "TWD", true)))
	assert.Equal(t, int64(100), f.balances.Of(domain.PSPReceivable("stripe", "TWD", true)))

	// 第二個爭議勝訴（政策退費）
	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_OPENED, &paymentv1.DisputeOpened{DisputeId: "dp_2", Amount: pmoney(50), Fee: pmoney(10), Provider: "stripe"})
	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_WON, &paymentv1.DisputeClosed{DisputeId: "dp_2", Amount: pmoney(50), Fee: pmoney(10), Provider: "stripe", Outcome: providerv1.DisputeOutcome_DISPUTE_OUTCOME_WON})
	assert.Equal(t, int64(662-600-450), payable(), "won dispute restores amount and fee")

	// 不記帳事件也能穿插
	handle(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_EVIDENCE_SUBMITTED, nil)

	require.NoError(t, f.balances.CheckIdentity())
	assert.Len(t, f.outbox, len(f.journals))
}

func TestHandlePaymentEvent_RefundFailedWithoutPendingIsUnlinked(t *testing.T) {
	svc, f := newTestService(domain.Policy{})
	rec, _ := envelope(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_FAILED, merchantA,
		&paymentv1.RefundFailed{RefundId: "re_x", Amount: pmoney(100), Provider: "stripe"})
	require.NoError(t, svc.HandlePaymentEvent(context.Background(), rec))
	require.Len(t, f.journals, 1)
	assert.Nil(t, f.journals[0].ReversalOf)
}

func TestEventTypeFromProto(t *testing.T) {
	assert.Equal(t, domain.EventPaymentCaptured, EventTypeFromProto(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED))
	assert.Equal(t, domain.EventPaymentRequiresAction, EventTypeFromProto(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_REQUIRES_ACTION))
	assert.Equal(t, domain.EventDisputeEvidenceSubmitted, EventTypeFromProto(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_EVIDENCE_SUBMITTED))
	assert.Equal(t, domain.EventRefundSucceeded, EventTypeFromProto(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_SUCCEEDED))
}

func TestFromProtoPaymentEvent_TestMode(t *testing.T) {
	rec, _ := envelope(t, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, merchantA,
		&paymentv1.PaymentCaptured{Amount: pmoney(10), Provider: "mock"})
	var ev paymentv1.PaymentEvent
	require.NoError(t, proto.Unmarshal(rec.Value, &ev))
	ev.Livemode = false
	dev, err := FromProtoPaymentEvent(&ev, uuid.New())
	require.NoError(t, err)
	assert.False(t, dev.Livemode)
	assert.Equal(t, merchantA, dev.MerchantID)
	assert.Equal(t, int64(10), dev.Amount.AmountMinor)
	assert.True(t, dev.Fee.IsZero())
}

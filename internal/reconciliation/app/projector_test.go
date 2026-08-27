package app_test

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
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

func twd(n int64) *commonv1.Money { return &commonv1.Money{AmountMinor: n, Currency: "TWD"} }

func envelope(typ paymentv1.PaymentEventType, paymentID string, version int64) *paymentv1.PaymentEvent {
	return &paymentv1.PaymentEvent{
		EventId: ids.New(ids.PrefixEvent), EventType: typ, OccurredAt: timestamppb.New(now.Add(-time.Hour)),
		MerchantId: ids.Format(ids.PrefixMerchant, merchantID), PaymentId: paymentID, Livemode: true, PaymentVersion: version,
	}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	require.NoError(t, err)
	return b
}

func TestHandlePaymentEvent_Projection(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{ConsumerName: "recon.test"})
	ctx := context.Background()
	payID := ids.New(ids.PrefixPayment)
	payUUID, err := ids.ParseWithPrefix(payID, ids.PrefixPayment)
	require.NoError(t, err)

	// authorized → captured。
	auth := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_AUTHORIZED, payID, 2)
	auth.PaymentStatus = paymentv1.PaymentStatus_PAYMENT_STATUS_AUTHORIZED
	auth.Payload = &paymentv1.PaymentEvent_PaymentAuthorized{PaymentAuthorized: &paymentv1.PaymentAuthorized{
		Amount: twd(1000), Provider: "mock", ProviderReference: "mock_ch_1", Fee: twd(59),
	}}
	require.NoError(t, svc.HandlePaymentEvent(ctx, uuid.NewString(), mustMarshal(t, auth)))
	r := store.Records[payUUID]
	require.NotNil(t, r)
	assert.Equal(t, domain.RecordPayment, r.Kind)
	assert.Equal(t, payID, r.PublicID)
	assert.Equal(t, merchantID, r.MerchantID)
	assert.Equal(t, domain.StatusAuthorized, r.Status)
	assert.Equal(t, "mock_ch_1", r.ProviderReference)
	assert.False(t, r.IsSettleable())
	assert.Equal(t, 2, r.SourceSeq)
	require.NotNil(t, r.Fee)
	assert.Equal(t, int64(59), r.Fee.AmountMinor)

	captured := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, payID, 3)
	captured.PaymentStatus = paymentv1.PaymentStatus_PAYMENT_STATUS_CAPTURED
	captured.Payload = &paymentv1.PaymentEvent_PaymentCaptured{PaymentCaptured: &paymentv1.PaymentCaptured{
		Amount: twd(800), TotalCapturedAmount: twd(800), Provider: "mock", ProviderReference: "mock_ch_1", Fee: twd(53), IsFinal: true,
	}}
	require.NoError(t, svc.HandlePaymentEvent(ctx, uuid.NewString(), mustMarshal(t, captured)))
	r = store.Records[payUUID]
	assert.Equal(t, domain.StatusCaptured, r.Status)
	assert.Equal(t, int64(800), r.Amount.AmountMinor, "以累計已請款為準")
	assert.True(t, r.IsSettleable())
	assert.Equal(t, 3, r.SourceSeq)

	// 亂序：較舊的 authorized（version 2）再到 → 丟棄。
	require.NoError(t, svc.HandlePaymentEvent(ctx, uuid.NewString(), mustMarshal(t, auth)))
	assert.Equal(t, domain.StatusCaptured, store.Records[payUUID].Status)

	// 同一 event_id 重送 → 去重（即使 payload 不同也不套用）。
	dupID := uuid.NewString()
	voided := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_VOIDED, payID, 9)
	voided.Payload = &paymentv1.PaymentEvent_PaymentVoided{PaymentVoided: &paymentv1.PaymentVoided{Amount: twd(800), Provider: "mock", ProviderReference: "mock_ch_1"}}
	require.NoError(t, svc.HandlePaymentEvent(ctx, dupID, mustMarshal(t, captured)))
	require.NoError(t, svc.HandlePaymentEvent(ctx, dupID, mustMarshal(t, voided)))
	assert.Equal(t, domain.StatusCaptured, store.Records[payUUID].Status)
	assert.True(t, store.Processed[dupID+"|recon.test"])

	// 退款：created（pending，無參照）→ succeeded。
	refID := ids.New(ids.PrefixRefund)
	refUUID, err := ids.ParseWithPrefix(refID, ids.PrefixRefund)
	require.NoError(t, err)
	rc := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_CREATED, payID, 4)
	rc.Payload = &paymentv1.PaymentEvent_RefundCreated{RefundCreated: &paymentv1.RefundCreated{RefundId: refID, Amount: twd(300), Provider: "mock"}}
	require.NoError(t, svc.HandlePaymentEvent(ctx, uuid.NewString(), mustMarshal(t, rc)))
	assert.Equal(t, domain.RefundPending, store.Records[refUUID].Status)
	assert.Empty(t, store.Records[refUUID].ProviderReference)

	rs := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_SUCCEEDED, payID, 5)
	rs.Payload = &paymentv1.PaymentEvent_RefundSucceeded{RefundSucceeded: &paymentv1.RefundSucceeded{RefundId: refID, Amount: twd(300), Provider: "mock", ProviderReference: "mock_re_1", Fee: twd(0)}}
	require.NoError(t, svc.HandlePaymentEvent(ctx, uuid.NewString(), mustMarshal(t, rs)))
	assert.Equal(t, domain.RefundSucceeded, store.Records[refUUID].Status)
	assert.Equal(t, "mock_re_1", store.Records[refUUID].ProviderReference)
	assert.True(t, store.Records[refUUID].IsSettleable())

	// 爭議：opened → lost。
	dpID := ids.New(ids.PrefixDispute)
	dpUUID, err := ids.ParseWithPrefix(dpID, ids.PrefixDispute)
	require.NoError(t, err)
	do := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_OPENED, payID, 6)
	do.Payload = &paymentv1.PaymentEvent_DisputeOpened{DisputeOpened: &paymentv1.DisputeOpened{DisputeId: dpID, Amount: twd(800), Provider: "mock", ProviderReference: "mock_du_1", Fee: twd(450)}}
	require.NoError(t, svc.HandlePaymentEvent(ctx, uuid.NewString(), mustMarshal(t, do)))
	assert.Equal(t, domain.DisputeOpen, store.Records[dpUUID].Status)

	dl := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_LOST, payID, 7)
	dl.Payload = &paymentv1.PaymentEvent_DisputeClosed{DisputeClosed: &paymentv1.DisputeClosed{DisputeId: dpID, Amount: twd(800), Provider: "mock", ProviderReference: "mock_du_1", Outcome: providerv1.DisputeOutcome_DISPUTE_OUTCOME_LOST}}
	require.NoError(t, svc.HandlePaymentEvent(ctx, uuid.NewString(), mustMarshal(t, dl)))
	assert.Equal(t, domain.DisputeLost, store.Records[dpUUID].Status)
	assert.True(t, store.Records[dpUUID].IsSettleable())

	dw := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_WON, payID, 8)
	dw.Payload = &paymentv1.PaymentEvent_DisputeClosed{DisputeClosed: &paymentv1.DisputeClosed{DisputeId: dpID, Amount: twd(800), Provider: "mock", ProviderReference: "mock_du_1", Outcome: providerv1.DisputeOutcome_DISPUTE_OUTCOME_WON}}
	require.NoError(t, svc.HandlePaymentEvent(ctx, uuid.NewString(), mustMarshal(t, dw)))
	assert.Equal(t, domain.DisputeWon, store.Records[dpUUID].Status)
}

func TestHandlePaymentEvent_IrrelevantAndPoison(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{})
	ctx := context.Background()

	// 不相關事件：只記錄去重。
	created := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CREATED, ids.New(ids.PrefixPayment), 1)
	created.Payload = &paymentv1.PaymentEvent_PaymentCreated{PaymentCreated: &paymentv1.PaymentCreated{Amount: twd(1)}}
	id := uuid.NewString()
	require.NoError(t, svc.HandlePaymentEvent(ctx, id, mustMarshal(t, created)))
	assert.Empty(t, store.Records)
	assert.Len(t, store.Processed, 1)

	// 沒有 provider_reference 的 failed 事件也略過。
	failed := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_FAILED, ids.New(ids.PrefixPayment), 1)
	failed.Payload = &paymentv1.PaymentEvent_PaymentFailed{PaymentFailed: &paymentv1.PaymentFailed{Amount: twd(1)}}
	require.NoError(t, svc.HandlePaymentEvent(ctx, uuid.NewString(), mustMarshal(t, failed)))
	assert.Empty(t, store.Records)

	// poison：反序列化失敗 → 回傳 ErrPoisonMessage，且不留下去重紀錄（fake 無 rollback，但錯誤必須上拋）。
	err := svc.HandlePaymentEvent(ctx, uuid.NewString(), []byte{0xff, 0xff, 0xff})
	require.Error(t, err)
	var poison *app.ErrPoisonMessage
	require.ErrorAs(t, err, &poison)

	// 缺 event_id。
	err = svc.HandlePaymentEvent(ctx, "", mustMarshal(t, created))
	assert.ErrorAs(t, err, &poison)
}

func TestProjectPaymentEvent_NonStandardIDs(t *testing.T) {
	// 測試資料常用非 ULID 的 id：以 uuid v5 穩定映射。
	ev := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, "pay_test_1", 1)
	ev.MerchantId = "mch_test"
	ev.Payload = &paymentv1.PaymentEvent_PaymentCaptured{PaymentCaptured: &paymentv1.PaymentCaptured{Amount: twd(10), Provider: "mock", ProviderReference: "x"}}
	r1, ok := app.ProjectPaymentEvent(ev, now)
	require.True(t, ok)
	r2, _ := app.ProjectPaymentEvent(ev, now)
	assert.Equal(t, r1.ID, r2.ID)
	assert.Equal(t, r1.MerchantID, r2.MerchantID)
	assert.NotEqual(t, uuid.Nil, r1.ID)
	assert.NotEqual(t, uuid.Nil, r1.MerchantID)

	// 沒有金額幣別 → 不投影。
	ev.Payload = &paymentv1.PaymentEvent_PaymentCaptured{PaymentCaptured: &paymentv1.PaymentCaptured{Provider: "mock", ProviderReference: "x"}}
	_, ok = app.ProjectPaymentEvent(ev, now)
	assert.False(t, ok)

	// 沒有 occurred_at → 用 now。
	ev2 := envelope(paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED, "pay_test_2", 1)
	ev2.OccurredAt = nil
	ev2.Payload = &paymentv1.PaymentEvent_PaymentCaptured{PaymentCaptured: &paymentv1.PaymentCaptured{Amount: twd(10), Provider: "mock", ProviderReference: "y"}}
	r3, ok := app.ProjectPaymentEvent(ev2, now)
	require.True(t, ok)
	assert.Equal(t, now, r3.OccurredAt)
}

package kafka

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

type fakeIngester struct {
	calls []*paymentv1.PaymentEvent
	err   error
}

func (f *fakeIngester) IngestPaymentEvent(_ context.Context, pe *paymentv1.PaymentEvent) (app.IngestResult, error) {
	f.calls = append(f.calls, pe)
	if f.err != nil {
		return app.IngestResult{}, f.err
	}
	return app.IngestResult{Deliveries: 1}, nil
}

func TestHandler(t *testing.T) {
	pe := &paymentv1.PaymentEvent{
		EventId: ids.New(ids.PrefixEvent), EventType: paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED,
		MerchantId: ids.New(ids.PrefixMerchant), PaymentId: "pay_1",
		Payload: &paymentv1.PaymentEvent_PaymentCaptured{PaymentCaptured: &paymentv1.PaymentCaptured{Amount: &commonv1.Money{AmountMinor: 1, Currency: "TWD"}}},
	}
	val, err := proto.Marshal(pe)
	require.NoError(t, err)
	rec := eventbus.Record{Topic: "payment.events", Value: val, Headers: map[string]string{eventbus.HeaderEventID: pe.EventId}}

	ing := &fakeIngester{}
	h := NewHandler(ing, nil, true)
	require.NoError(t, h.Handle(context.Background(), rec))
	require.Len(t, ing.calls, 1)
	assert.Equal(t, pe.EventId, ing.calls[0].EventId)

	// 暫時性錯誤 → 回錯誤（不 commit）。
	ing.err = errors.New("db down")
	require.Error(t, h.Handle(context.Background(), rec))

	// 不可重試錯誤：skipPoison=true 略過；false 回錯誤（→ DLQ）。
	ing.err = domain.ErrUnsupportedEvent
	require.NoError(t, h.Handle(context.Background(), rec))
	require.Error(t, NewHandler(ing, nil, false).Handle(context.Background(), rec))

	// 壞 payload。
	bad := eventbus.Record{Topic: "payment.events", Value: []byte{0xff, 0xff, 0xff}}
	require.NoError(t, NewHandler(ing, nil, true).Handle(context.Background(), bad))
	assert.Error(t, NewHandler(ing, nil, false).Handle(context.Background(), bad))
}

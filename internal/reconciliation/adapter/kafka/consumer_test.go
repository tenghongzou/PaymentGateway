package kafka

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

type spyHandler struct {
	ids  []string
	errs []error
}

func (s *spyHandler) HandlePaymentEvent(_ context.Context, eventID string, _ []byte) error {
	s.ids = append(s.ids, eventID)
	if len(s.errs) > 0 {
		e := s.errs[0]
		s.errs = s.errs[1:]
		return e
	}
	return nil
}

func TestEventUUID(t *testing.T) {
	u := uuid.New()
	assert.Equal(t, u.String(), EventUUID(u.String(), ""))
	evt := ids.New(ids.PrefixEvent)
	_, want, err := ids.Parse(evt)
	require.NoError(t, err)
	assert.Equal(t, want.String(), EventUUID(evt, ""))
	assert.Equal(t, want.String(), EventUUID("", evt))
	// 非 uuid 的自訂 event_id：以 v5 雜湊穩定映射。
	assert.Equal(t, uuid.NewSHA1(uuid.NameSpaceOID, []byte("custom-1")).String(), EventUUID("custom-1", ""))
	assert.NotEmpty(t, EventUUID("custom-1", ""))
	assert.Empty(t, EventUUID("", ""))
}

func TestConsumer_Handle(t *testing.T) {
	spy := &spyHandler{}
	c := &Consumer{handler: spy, log: discardLogger()}

	// header 有 event_id。
	u := uuid.New()
	require.NoError(t, c.Handle(context.Background(), eventbus.Record{Headers: map[string]string{eventbus.HeaderEventID: u.String()}, Value: []byte("x")}))
	assert.Equal(t, u.String(), spy.ids[0])

	// header 缺 event_id → 從 payload 取。
	evt := ids.New(ids.PrefixEvent)
	b, err := proto.Marshal(&paymentv1.PaymentEvent{EventId: evt})
	require.NoError(t, err)
	require.NoError(t, c.Handle(context.Background(), eventbus.Record{Value: b}))
	_, want, err := ids.Parse(evt)
	require.NoError(t, err)
	assert.Equal(t, want.String(), spy.ids[1])

	// poison 錯誤原樣上拋（eventbus 會重試後送 DLQ）。
	spy.errs = []error{&app.ErrPoisonMessage{Err: errors.New("bad")}}
	err = c.Handle(context.Background(), eventbus.Record{Value: []byte{0xff}})
	var poison *app.ErrPoisonMessage
	assert.ErrorAs(t, err, &poison)
}

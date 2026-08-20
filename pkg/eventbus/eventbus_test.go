package eventbus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tenghongzou/paymentgateway/pkg/outbox"
)

var _ outbox.Publisher = (*Producer)(nil)

func TestHeaderConversion(t *testing.T) {
	hs := toHeaders(map[string]string{"event_id": "e1", "event_type": "payment.created"})
	require.Len(t, hs, 2)
	rec := fromKgo(&kgo.Record{Topic: TopicPaymentEvents, Key: []byte("pay_1"), Value: []byte("v"), Headers: hs, Partition: 2, Offset: 9})
	assert.Equal(t, "e1", rec.EventID())
	assert.Equal(t, "payment.created", rec.Headers[HeaderEventType])
	assert.Equal(t, "pay_1", rec.Key)
	assert.Equal(t, int32(2), rec.Partition)
	assert.Nil(t, toHeaders(nil))
}

func TestOptionsValidation(t *testing.T) {
	_, err := NewProducer(Options{})
	require.Error(t, err)
	_, err = NewConsumer(ConsumerConfig{Options: Options{Brokers: []string{"localhost:1"}}})
	require.Error(t, err)
	_, err = NewConsumer(ConsumerConfig{Options: Options{Brokers: []string{"localhost:1"}}, Group: "g"})
	require.Error(t, err)
}

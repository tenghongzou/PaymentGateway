package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestEndpointSubscribes(t *testing.T) {
	tests := []struct {
		name    string
		enabled []string
		event   string
		want    bool
	}{
		{"empty list = all", nil, "payment.captured", true},
		{"wildcard", []string{"*"}, "refund.failed", true},
		{"exact match", []string{"payment.captured", "refund.succeeded"}, "payment.captured", true},
		{"exact miss", []string{"payment.captured"}, "payment.failed", false},
		{"prefix glob", []string{"payment.*"}, "payment.failed", true},
		{"prefix glob miss", []string{"payment.*"}, "refund.failed", false},
		{"whitespace tolerant", []string{" payment.captured "}, "payment.captured", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := &Endpoint{EnabledEvents: tt.enabled}
			assert.Equal(t, tt.want, ep.Subscribes(tt.event))
		})
	}
}

func TestEndpointAcceptsAndFanOut(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	m := uuid.New()
	ev := &Event{ID: uuid.New(), MerchantID: m, Type: "payment.captured", Livemode: true, Payload: []byte(`{}`), OccurredAt: now}
	eps := []*Endpoint{
		{ID: uuid.New(), MerchantID: m, Status: EndpointEnabled, Livemode: true, EnabledEvents: []string{"*"}},                // 收
		{ID: uuid.New(), MerchantID: m, Status: EndpointDisabled, Livemode: true},                                             // 停用
		{ID: uuid.New(), MerchantID: m, Status: EndpointEnabled, Livemode: false},                                             // test mode
		{ID: uuid.New(), MerchantID: m, Status: EndpointEnabled, Livemode: true, EnabledEvents: []string{"refund.succeeded"}}, // 未訂閱
		{ID: uuid.New(), MerchantID: m, Status: EndpointEnabled, Livemode: true, EnabledEvents: []string{"payment.*"}},        // 收
		{ID: uuid.New(), MerchantID: m, Status: EndpointDeleted, Livemode: true},                                              // 刪除
	}
	ds := FanOut(ev, eps, func() time.Time { return now })
	assert.Len(t, ds, 2)
	assert.Equal(t, eps[0].ID, ds[0].EndpointID)
	assert.Equal(t, eps[4].ID, ds[1].EndpointID)
	for _, d := range ds {
		assert.Equal(t, StatusPending, d.Status)
		assert.Equal(t, 0, d.AttemptNo)
		assert.Equal(t, now, d.NextAttemptAt)
		assert.Equal(t, ev.ID, d.EventID)
		assert.Equal(t, m, d.MerchantID)
	}
	assert.Equal(t, []string{"a", "b"}, (&Endpoint{Secrets: []string{"a", "", "b"}}).ActiveSecrets())
}

package outbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memBatcher 以記憶體模擬 outbox 表。
type memBatcher struct {
	mu        sync.Mutex
	pending   []Message
	published []Message
	attempts  map[string]int
}

func (b *memBatcher) ProcessBatch(ctx context.Context, limit int, fn func(ctx context.Context, msgs []Message) []error) (int, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(limit, len(b.pending))
	if n == 0 {
		return 0, 0, nil
	}
	batch := append([]Message(nil), b.pending[:n]...)
	results := fn(ctx, batch)
	var remaining []Message
	failed := 0
	for i, m := range batch {
		b.attempts[m.ID]++
		if results[i] != nil {
			failed++
			remaining = append(remaining, m)
			continue
		}
		b.published = append(b.published, m)
	}
	b.pending = append(remaining, b.pending[n:]...)
	return n, failed, nil
}

type memPublisher struct {
	mu     sync.Mutex
	sent   []sent
	failFn func(key string) error
}

type sent struct {
	topic, key string
	value      []byte
	headers    map[string]string
}

func (p *memPublisher) Publish(_ context.Context, topic, key string, value []byte, headers map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failFn != nil {
		if err := p.failFn(key); err != nil {
			return err
		}
	}
	p.sent = append(p.sent, sent{topic, key, value, headers})
	return nil
}

func TestRelayPublishesAndRetries(t *testing.T) {
	b := &memBatcher{attempts: map[string]int{}}
	for _, id := range []string{"e1", "e2", "e3"} {
		b.pending = append(b.pending, Message{ID: id, AggregateType: "payment", AggregateID: "pay_" + id, EventType: "payment.created", Payload: []byte(id), Headers: map[string]string{"merchant_id": "m1"}})
	}
	var failOnce sync.Once
	p := &memPublisher{}
	p.failFn = func(key string) error {
		var err error
		if key == "pay_e2" {
			failOnce.Do(func() { err = errors.New("kafka down") })
		}
		return err
	}
	r := NewRelay(RelayConfig{Batcher: b, Publisher: p, Topic: func(Message) string { return "payment.events" }, BatchSize: 2, PollInterval: time.Millisecond, MaxBackoff: 5 * time.Millisecond})

	// 第一批：e1 成功、e2 失敗。
	total, failed, err := r.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Equal(t, 1, failed)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.published) == 3
	}, 2*time.Second, 2*time.Millisecond)
	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, 2, b.attempts["e2"], "e2 should have been retried once")
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Len(t, p.sent, 3)
	assert.Equal(t, "payment.events", p.sent[0].topic)
	assert.Equal(t, "pay_e1", p.sent[0].key)
	assert.Equal(t, "e1", p.sent[0].headers["event_id"])
	assert.Equal(t, "payment.created", p.sent[0].headers["event_type"])
	assert.Equal(t, "m1", p.sent[0].headers["merchant_id"])
}

func TestRelayBackoff(t *testing.T) {
	r := NewRelay(RelayConfig{PollInterval: 10 * time.Millisecond, MaxBackoff: 35 * time.Millisecond, BatchSize: 10})
	log := r.cfg.Logger
	assert.Equal(t, 10*time.Millisecond, r.nextDelay(1, 1, nil, log))
	assert.Equal(t, 20*time.Millisecond, r.nextDelay(1, 1, nil, log))
	assert.Equal(t, 35*time.Millisecond, r.nextDelay(0, 0, errors.New("db"), log))
	assert.Equal(t, 35*time.Millisecond, r.nextDelay(0, 0, errors.New("db"), log))
	// 成功後重置；批次滿立即再跑；否則 poll interval。
	assert.Equal(t, time.Duration(0), r.nextDelay(10, 0, nil, log))
	assert.Equal(t, 10*time.Millisecond, r.nextDelay(3, 0, nil, log))
	assert.Equal(t, "abc", truncate("abcdef", 3))
	assert.Equal(t, "ab", truncate("ab", 3))
}

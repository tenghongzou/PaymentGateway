//go:build integration

package eventbus

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
)

func TestProduceConsume(t *testing.T) {
	ctx := context.Background()
	kc, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.5.0", tckafka.WithClusterID("test-cluster"))
	require.NoError(t, err)
	testcontainers.CleanupContainer(t, kc)
	brokers, err := kc.Brokers(ctx)
	require.NoError(t, err)

	p, err := NewProducer(Options{Brokers: brokers, ClientID: "test"})
	require.NoError(t, err)
	defer p.Close(ctx)
	require.NoError(t, p.Publish(ctx, "it.events", "k1", []byte("v1"), map[string]string{HeaderEventID: "e1"}))

	c, err := NewConsumer(ConsumerConfig{Options: Options{Brokers: brokers, ClientID: "test-c"}, Group: "g1", Topics: []string{"it.events"}})
	require.NoError(t, err)
	got := make(chan Record, 1)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- c.Run(cctx, func(_ context.Context, r Record) error {
			got <- r
			cancel()
			return nil
		})
	}()
	defer func() { <-runErr }()
	select {
	case r := <-got:
		assert.Equal(t, "k1", r.Key)
		assert.Equal(t, "e1", r.EventID())
	case <-cctx.Done():
		t.Fatal("timed out waiting for record")
	}
}

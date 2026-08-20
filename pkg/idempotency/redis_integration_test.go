//go:build integration

package idempotency

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func TestRedisStoreFlow(t *testing.T) {
	ctx := context.Background()
	c, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	testcontainers.CleanupContainer(t, c)
	uri, err := c.ConnectionString(ctx)
	require.NoError(t, err)
	opt, err := redis.ParseURL(uri)
	require.NoError(t, err)
	rdb := redis.NewClient(opt)
	defer rdb.Close()

	s := NewRedisStore(rdb, 0)
	st, _, err := s.Begin(ctx, "m", "k", "h")
	require.NoError(t, err)
	assert.Equal(t, StateNew, st)
	_, _, err = s.Begin(ctx, "m", "k", "h")
	require.ErrorIs(t, err, ErrInProgress)
	_, _, err = s.Begin(ctx, "m", "k", "x")
	require.ErrorIs(t, err, ErrMismatch)
	require.NoError(t, s.Complete(ctx, "m", "k", Response{StatusCode: 201, Body: []byte("{}")}))
	st, resp, err := s.Begin(ctx, "m", "k", "h")
	require.NoError(t, err)
	assert.Equal(t, StateCompleted, st)
	assert.Equal(t, 201, resp.StatusCode)
	require.NoError(t, s.Abort(ctx, "m", "k"))
	st, _, err = s.Begin(ctx, "m", "k", "h2")
	require.NoError(t, err)
	assert.Equal(t, StateNew, st)
}

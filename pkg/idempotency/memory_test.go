package idempotency

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStoreFlow(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	s := NewMemoryStore().WithClock(func() time.Time { return now })

	st, resp, err := s.Begin(ctx, "mch_1", "k1", "h1")
	require.NoError(t, err)
	assert.Equal(t, StateNew, st)
	assert.Nil(t, resp)

	// 處理中：同 hash → ErrInProgress；不同 hash → ErrMismatch。
	_, _, err = s.Begin(ctx, "mch_1", "k1", "h1")
	require.ErrorIs(t, err, ErrInProgress)
	_, _, err = s.Begin(ctx, "mch_1", "k1", "h2")
	require.ErrorIs(t, err, ErrMismatch)

	// 不同商戶互不影響。
	st, _, err = s.Begin(ctx, "mch_2", "k1", "h1")
	require.NoError(t, err)
	assert.Equal(t, StateNew, st)

	require.NoError(t, s.Complete(ctx, "mch_1", "k1", Response{StatusCode: 201, Body: []byte(`{"id":"pay_1"}`)}))
	st, resp, err = s.Begin(ctx, "mch_1", "k1", "h1")
	require.NoError(t, err)
	assert.Equal(t, StateCompleted, st)
	require.NotNil(t, resp)
	assert.Equal(t, 201, resp.StatusCode)
	assert.JSONEq(t, `{"id":"pay_1"}`, string(resp.Body))

	_, _, err = s.Begin(ctx, "mch_1", "k1", "h2")
	require.ErrorIs(t, err, ErrMismatch)

	// 24h 後過期 → 可重新開始。
	now = now.Add(DefaultTTL + time.Second)
	st, _, err = s.Begin(ctx, "mch_1", "k1", "h3")
	require.NoError(t, err)
	assert.Equal(t, StateNew, st)

	// Abort 釋放鎖。
	require.NoError(t, s.Abort(ctx, "mch_1", "k1"))
	st, _, err = s.Begin(ctx, "mch_1", "k1", "h4")
	require.NoError(t, err)
	assert.Equal(t, StateNew, st)

	// 鎖 30s 過期後視同可重新開始（gateway 崩潰情境）。
	now = now.Add(DefaultLockTTL + time.Second)
	st, _, err = s.Begin(ctx, "mch_1", "k1", "h5")
	require.NoError(t, err)
	assert.Equal(t, StateNew, st)
}

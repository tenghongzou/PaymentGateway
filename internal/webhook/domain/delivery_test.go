package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackoffSchedule(t *testing.T) {
	// docs/06 §4.4：1 立即、2 1m、3 5m、4 30m、5 2h、6 6h、7 12h、8 24h、9 24h、10 24h。
	want := map[int]time.Duration{
		1: 0, 2: time.Minute, 3: 5 * time.Minute, 4: 30 * time.Minute, 5: 2 * time.Hour,
		6: 6 * time.Hour, 7: 12 * time.Hour, 8: 24 * time.Hour, 9: 24 * time.Hour, 10: 24 * time.Hour,
		11: 24 * time.Hour, 0: 0,
	}
	for n, d := range want {
		assert.Equal(t, d, Backoff(n), "attempt %d", n)
	}
	// 累計約 3d20h36m。
	var total time.Duration
	for n := 1; n <= MaxAttempts; n++ {
		total += Backoff(n)
	}
	assert.Equal(t, 3*24*time.Hour+20*time.Hour+36*time.Minute, total)
}

func TestNextAttemptAtJitter(t *testing.T) {
	now := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	// 第 1 次失敗 → 第 2 次間隔 1m；rnd=0.5 無 jitter。
	assert.Equal(t, now.Add(time.Minute), NextAttemptAt(now, 1, 0.5))
	// ±20% 範圍。
	lo := NextAttemptAt(now, 3, 0)
	hi := NextAttemptAt(now, 3, 0.999999)
	assert.Equal(t, now.Add(24*time.Minute), lo)
	assert.WithinDuration(t, now.Add(36*time.Minute), hi, time.Second)
	// 第 4 次失敗 → 2h 區間。
	for _, rnd := range []float64{0, 0.25, 0.5, 0.75, 0.99} { //nolint:forbidigo // jitter 亂數，非金額
		got := NextAttemptAt(now, 4, rnd)
		assert.False(t, got.Before(now.Add(96*time.Minute)), "rnd=%v", rnd)
		assert.False(t, got.After(now.Add(144*time.Minute)), "rnd=%v", rnd)
	}
}

func newTestDelivery(now time.Time) *Delivery {
	ev := &Event{ID: uuid.New(), MerchantID: uuid.New(), Type: "payment.captured", Livemode: true, Payload: []byte(`{}`), OccurredAt: now}
	ep := &Endpoint{ID: uuid.New(), MerchantID: ev.MerchantID, URL: "https://example.com/hook", Status: EndpointEnabled, Livemode: true}
	return NewDelivery(ev, ep, now)
}

func TestDeliveryLifecycle_SuccessFirstTry(t *testing.T) {
	now := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	d := newTestDelivery(now)
	require.True(t, d.IsDue(now))
	require.NoError(t, d.Claim(now))
	assert.Equal(t, StatusInFlight, d.Status)
	assert.Equal(t, 1, d.AttemptNo)

	tr, att, err := d.ApplyOutcome(now.Add(time.Second), Outcome{StatusCode: 200, Body: "ok", Duration: 120 * time.Millisecond}, 0.5)
	require.NoError(t, err)
	assert.Equal(t, TransitionSucceeded, tr)
	assert.Equal(t, StatusSucceeded, d.Status)
	require.NotNil(t, d.DeliveredAt)
	assert.Equal(t, 1, att.AttemptNo)
	assert.True(t, att.Succeeded())
	assert.Equal(t, 120, att.DurationMS)
	// 終態不可再取件。
	assert.False(t, d.IsDue(now.Add(time.Hour)))
	assert.ErrorIs(t, d.Claim(now.Add(time.Hour)), ErrInvalidTransition)
}

func TestDeliveryLifecycle_TenFailuresDeadLetter(t *testing.T) {
	now := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	d := newTestDelivery(now)
	cur := now
	for n := 1; n <= MaxAttempts; n++ {
		require.True(t, d.IsDue(cur), "attempt %d should be due", n)
		require.NoError(t, d.Claim(cur))
		assert.Equal(t, n, d.AttemptNo)
		tr, att, err := d.ApplyOutcome(cur, Outcome{StatusCode: 503, Body: "unavailable"}, 0.5)
		require.NoError(t, err)
		assert.Equal(t, n, att.AttemptNo)
		if n < MaxAttempts {
			assert.Equal(t, TransitionRetry, tr)
			assert.Equal(t, StatusFailed, d.Status)
			assert.Equal(t, cur.Add(Backoff(n+1)), d.NextAttemptAt, "attempt %d", n)
			assert.False(t, d.IsDue(cur), "must wait for backoff")
			cur = d.NextAttemptAt
		} else {
			assert.Equal(t, TransitionDeadLetter, tr)
			assert.Equal(t, StatusDeadLetter, d.Status)
		}
	}
	assert.Equal(t, 503, *d.LastResponseStatus)
	assert.Equal(t, "unavailable", *d.LastResponseBody)
	// 死信可手動重送並重置視窗。
	require.NoError(t, d.ResetForRetry(cur))
	assert.Equal(t, StatusPending, d.Status)
	assert.Equal(t, 0, d.AttemptNo)
	assert.True(t, d.IsDue(cur))
}

func TestDeliveryLifecycle_GoneCancels(t *testing.T) {
	now := time.Now().UTC()
	d := newTestDelivery(now)
	require.NoError(t, d.Claim(now))
	tr, _, err := d.ApplyOutcome(now, Outcome{StatusCode: 410}, 0.5)
	require.NoError(t, err)
	assert.Equal(t, TransitionGone, tr)
	assert.Equal(t, StatusCanceled, d.Status)
	assert.ErrorIs(t, d.ResetForRetry(now), ErrDeliveryNotRetryable)
}

func TestDeliveryLifecycle_RetryAfterCapped(t *testing.T) {
	now := time.Now().UTC()
	d := newTestDelivery(now)
	require.NoError(t, d.Claim(now))
	tr, _, err := d.ApplyOutcome(now, Outcome{StatusCode: 429, RetryAfter: 5 * time.Hour}, 0.5)
	require.NoError(t, err)
	assert.Equal(t, TransitionRetry, tr)
	assert.Equal(t, now.Add(MaxRetryAfter), d.NextAttemptAt)

	d2 := newTestDelivery(now)
	require.NoError(t, d2.Claim(now))
	_, _, err = d2.ApplyOutcome(now, Outcome{StatusCode: 429, RetryAfter: 30 * time.Second}, 0.5)
	require.NoError(t, err)
	assert.Equal(t, now.Add(30*time.Second), d2.NextAttemptAt)
}

func TestDeliveryLifecycle_ConnectionErrorAndTruncation(t *testing.T) {
	now := time.Now().UTC()
	d := newTestDelivery(now)
	require.NoError(t, d.Claim(now))
	_, att, err := d.ApplyOutcome(now, Outcome{Err: errors.New("dial tcp: i/o timeout")}, 0.5)
	require.NoError(t, err)
	assert.Nil(t, att.ResponseStatus)
	assert.Equal(t, "dial tcp: i/o timeout", *att.Error)
	assert.Equal(t, StatusFailed, d.Status)

	big := strings.Repeat("a", 5000) + "中"
	assert.LessOrEqual(t, len(TruncateBody(big)), MaxResponseBodyBytes)
	assert.Len(t, TruncateBody(big), 4096)
	// 多位元組字元被截斷時不留下半個字。
	s := strings.Repeat("a", 4095) + "中文"
	out := TruncateBody(s)
	assert.LessOrEqual(t, len(out), 4096)
	assert.Equal(t, strings.Repeat("a", 4095), out)
}

func TestDeliveryReapAndCancel(t *testing.T) {
	now := time.Now().UTC()
	d := newTestDelivery(now)
	require.ErrorIs(t, d.Reap(now), ErrInvalidTransition)
	require.NoError(t, d.Claim(now))
	require.NoError(t, d.Reap(now.Add(3*time.Minute)))
	assert.Equal(t, StatusFailed, d.Status)
	assert.True(t, d.IsDue(now.Add(3*time.Minute)))
	assert.True(t, d.Cancel(now, "endpoint deleted"))
	assert.Equal(t, StatusCanceled, d.Status)
	assert.False(t, d.Cancel(now, "again"))
}

func TestApplyOutcomeRequiresInFlight(t *testing.T) {
	d := newTestDelivery(time.Now())
	_, _, err := d.ApplyOutcome(time.Now(), Outcome{StatusCode: 200}, 0.5)
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

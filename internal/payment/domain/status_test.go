package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCanTransitionTable 以 docs/02 §3.2 的轉移表做全矩陣驗證：表中列出的為合法，其餘全部非法。
func TestCanTransitionTable(t *testing.T) {
	legal := map[string]bool{}
	for _, tr := range []struct{ from, to Status }{
		{StatusCreated, StatusRequiresAction},              // T1
		{StatusCreated, StatusAuthorized},                  // T2
		{StatusCreated, StatusCaptured},                    // T3
		{StatusCreated, StatusFailed},                      // T4
		{StatusCreated, StatusExpired},                     // T5
		{StatusRequiresAction, StatusAuthorized},           // T6
		{StatusRequiresAction, StatusCaptured},             // T7
		{StatusRequiresAction, StatusFailed},               // T8
		{StatusRequiresAction, StatusExpired},              // T9
		{StatusRequiresAction, StatusVoided},               // T10
		{StatusAuthorized, StatusCaptured},                 // T11
		{StatusAuthorized, StatusVoided},                   // T12 / T13
		{StatusCaptured, StatusPartiallyRefunded},          // T14
		{StatusCaptured, StatusRefunded},                   // T15
		{StatusPartiallyRefunded, StatusPartiallyRefunded}, // T16
		{StatusPartiallyRefunded, StatusRefunded},          // T17
		{StatusCaptured, StatusDisputed},                   // T18
		{StatusPartiallyRefunded, StatusDisputed},
		{StatusRefunded, StatusDisputed},
		{StatusChargebackWon, StatusDisputed},
		{StatusDisputed, StatusChargebackWon},          // T19
		{StatusDisputed, StatusChargebackLost},         // T20
		{StatusChargebackWon, StatusPartiallyRefunded}, // T21
		{StatusChargebackWon, StatusRefunded},
	} {
		legal[string(tr.from)+"->"+string(tr.to)] = true
	}
	for _, from := range AllStatuses {
		for _, to := range AllStatuses {
			key := string(from) + "->" + string(to)
			t.Run(key, func(t *testing.T) {
				assert.Equal(t, legal[key], CanTransition(from, to), key)
			})
		}
	}
	// 終態沒有任何出口。
	for _, s := range AllStatuses {
		if s.IsTerminal() {
			for _, to := range AllStatuses {
				assert.False(t, CanTransition(s, to), "terminal %s must not transition to %s", s, to)
			}
		}
	}
	assert.False(t, CanTransition("bogus", StatusCreated))
	assert.True(t, Status("created").IsValid())
	assert.False(t, Status("bogus").IsValid())
}

func TestTerminal(t *testing.T) {
	for _, s := range []Status{StatusVoided, StatusFailed, StatusExpired, StatusChargebackLost} {
		assert.True(t, s.IsTerminal(), s)
	}
	for _, s := range []Status{StatusCreated, StatusRequiresAction, StatusAuthorized, StatusCaptured, StatusPartiallyRefunded, StatusRefunded, StatusDisputed, StatusChargebackWon} {
		assert.False(t, s.IsTerminal(), s)
	}
	assert.True(t, AttemptApproved.IsTerminal())
	assert.True(t, AttemptUnknown.IsOpen())
	assert.False(t, AttemptDeclined.IsOpen())
}

func TestProviderErrorCategoryTable(t *testing.T) {
	tests := []struct {
		cat      ProviderErrorCategory
		retry    bool
		failover bool
		attempt  AttemptStatus
		restCode string
	}{
		{CategoryDeclinedHard, false, false, AttemptDeclined, "card_declined"},
		{CategoryDeclinedSoft, true, true, AttemptDeclined, "card_declined"},
		{CategoryFraudSuspected, false, false, AttemptDeclined, "card_declined"},
		{CategoryAuthenticationRequired, false, false, AttemptRequiresAction, "authentication_required"},
		{CategoryAuthenticationFailed, false, false, AttemptDeclined, "card_declined"},
		{CategoryInvalidRequest, false, false, AttemptDeclined, "provider_rejected"},
		{CategoryProviderConfigError, false, true, AttemptUnavailable, "provider_unavailable"},
		{CategoryProviderUnavailable, false, true, AttemptUnavailable, "provider_unavailable"},
		{CategoryProviderRateLimited, true, true, AttemptUnavailable, "provider_unavailable"},
		{CategoryProviderTimeout, false, false, AttemptUnknown, "provider_timeout"},
		{CategoryDuplicateRequest, false, false, AttemptDeclined, "provider_rejected"},
		{CategoryUnsupportedOperation, false, false, AttemptDeclined, "provider_rejected"},
		{CategoryUnknown, false, false, AttemptUnknown, "provider_timeout"},
	}
	for _, tt := range tests {
		t.Run(string(tt.cat), func(t *testing.T) {
			assert.True(t, tt.cat.IsValid())
			assert.Equal(t, tt.retry, tt.cat.IsRetryable())
			assert.Equal(t, tt.failover, tt.cat.CanFailover())
			assert.Equal(t, tt.attempt, tt.cat.AttemptStatus())
			assert.Equal(t, tt.restCode, tt.cat.RESTCode())
		})
	}
	assert.False(t, ProviderErrorCategory("nope").IsValid())
	assert.Equal(t, AttemptUnknown, ProviderErrorCategory("nope").AttemptStatus())

	// declined_soft 白名單。
	assert.True(t, CanFailoverDecline(CategoryDeclinedSoft, "try_again_later"))
	assert.True(t, CanFailoverDecline(CategoryDeclinedSoft, "issuer_unavailable"))
	assert.False(t, CanFailoverDecline(CategoryDeclinedSoft, "velocity_exceeded"))
	assert.False(t, CanFailoverDecline(CategoryDeclinedHard, "insufficient_funds"))
	assert.False(t, CanFailoverDecline(CategoryDeclinedHard, "try_again_later"))
	assert.True(t, CanFailoverDecline(CategoryProviderUnavailable, ""))

	// retryable 語意。
	assert.True(t, IsRetryableDecline(CategoryDeclinedHard, "insufficient_funds"))
	assert.False(t, IsRetryableDecline(CategoryDeclinedHard, "do_not_honor"))
	assert.False(t, IsRetryableDecline(CategoryFraudSuspected, "fraudulent"))
	assert.True(t, IsRetryableDecline(CategoryProviderUnavailable, ""))
	assert.Equal(t, "generic_decline", Failure{Category: CategoryFraudSuspected, Code: "stolen_card"}.PublicCode())
	assert.Equal(t, "insufficient_funds", Failure{Category: CategoryDeclinedHard, Code: "insufficient_funds"}.PublicCode())
	assert.Equal(t, "provider_unavailable", Failure{Category: CategoryProviderUnavailable}.PublicCode())
}

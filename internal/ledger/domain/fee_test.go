package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

func TestFeePolicy_Calculate(t *testing.T) {
	tests := []struct {
		name      string
		policy    FeePolicy
		amount    money.Money
		wantTotal int64
		wantPct   int64
		clamped   bool
	}{
		// docs/02 §7.3 範例：2.8% + 5 元，1,000 元 → 5 + 28 = 33
		{"doc example", FeePolicy{FixedMinor: 5, PercentageBps: 280}, twd(1000), 33, 28, false},
		// half-up：1 元 × 2.9% = 0.029 → 0；15 元 × 2.9% = 0.435 → 0；17 × 290 = 4930 / 10000 = 0.493 → 0；
		// 18 × 290 = 5220 → 0.522 → 1
		{"half-up rounds up at .5", FeePolicy{PercentageBps: 5000}, twd(1), 1, 1, false}, // 0.5 → 1
		{"half-up rounds down below .5", FeePolicy{PercentageBps: 4999}, twd(1), 0, 0, false},
		{"usd cents", FeePolicy{FixedMinor: 30, PercentageBps: 290}, money.MustNew(10000, "USD"), 320, 290, false},
		{"min clamp", FeePolicy{PercentageBps: 100, MinFeeMinor: 50}, twd(1000), 50, 10, true},
		{"max clamp", FeePolicy{PercentageBps: 1000, MaxFeeMinor: 50}, twd(1000), 50, 100, true},
		{"fee cannot exceed amount", FeePolicy{FixedMinor: 500}, twd(100), 100, 0, true},
		{"zero amount", FeePolicy{FixedMinor: 5, PercentageBps: 280}, twd(0), 0, 0, true},
		{"no fee", FeePolicy{}, twd(1000), 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.policy.Calculate(tt.amount)
			require.NoError(t, err)
			assert.Equal(t, tt.wantTotal, got.Total.AmountMinor, "total")
			assert.Equal(t, tt.wantPct, got.PercentagePart.AmountMinor, "percentage part")
			assert.Equal(t, tt.amount.Currency, got.Total.Currency)
			assert.Equal(t, tt.clamped, got.Clamped, "clamped")
			assert.LessOrEqual(t, got.Total.AmountMinor, tt.amount.AmountMinor)
		})
	}

	_, err := FeePolicy{FixedMinor: -1}.Calculate(twd(10))
	require.Error(t, err)
	_, err = FeePolicy{}.Calculate(money.Money{AmountMinor: 1, Currency: "ZZZ"})
	assert.ErrorIs(t, err, ErrInvalidCurrency)
}

func TestProRataFee(t *testing.T) {
	// docs/02 §7.3 J-REF-FEE-RET：fee 33、退 300 / 1000 → round(9.9) = 10
	got, err := ProRataFee(twd(33), twd(300), twd(1000))
	require.NoError(t, err)
	assert.Equal(t, int64(10), got.AmountMinor)

	// half-up：fee 5、退 1/2 → 2.5 → 3
	got, err = ProRataFee(twd(5), twd(1), twd(2))
	require.NoError(t, err)
	assert.Equal(t, int64(3), got.AmountMinor)

	// 全額退 → 全額 fee
	got, err = ProRataFee(twd(33), twd(1000), twd(1000))
	require.NoError(t, err)
	assert.Equal(t, int64(33), got.AmountMinor)

	// part > total 視為 total
	got, err = ProRataFee(twd(33), twd(5000), twd(1000))
	require.NoError(t, err)
	assert.Equal(t, int64(33), got.AmountMinor)

	// 大數不溢位（fee × part 超過 int64）
	got, err = ProRataFee(twd(1<<62), twd(1<<62), twd(1<<62))
	require.NoError(t, err)
	assert.Equal(t, int64(1<<62), got.AmountMinor)

	// 零
	got, err = ProRataFee(twd(0), twd(1), twd(2))
	require.NoError(t, err)
	assert.True(t, got.IsZero())

	_, err = ProRataFee(twd(1), money.MustNew(1, "USD"), twd(2))
	assert.ErrorIs(t, err, ErrJournalCurrencyMismatch)
}

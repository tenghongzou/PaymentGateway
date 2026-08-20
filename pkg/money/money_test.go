package money

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
)

func TestNewAndValidate(t *testing.T) {
	tests := []struct {
		name     string
		amount   int64
		currency string
		wantErr  error
	}{
		{"twd ok", 100, "TWD", nil},
		{"usd ok", 0, "USD", nil},
		{"kwd ok", 1, "KWD", nil},
		{"negative", -1, "TWD", ErrNegativeAmount},
		{"lowercase", 1, "twd", ErrInvalidCurrency},
		{"too short", 1, "TW", ErrInvalidCurrency},
		{"unknown", 1, "XXX", ErrInvalidCurrency},
		{"empty", 1, "", ErrInvalidCurrency},
		{"digits", 1, "TW1", ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := New(tt.amount, tt.currency)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.amount, m.AmountMinor)
			assert.Equal(t, tt.currency, m.Currency)
		})
	}
}

func TestExponent(t *testing.T) {
	tests := map[string]int{
		"TWD": 0, "JPY": 0, "KRW": 0,
		"USD": 2, "EUR": 2, "GBP": 2, "SGD": 2, "HKD": 2, "CNY": 2,
		"KWD": 3, "BHD": 3,
		"XXX": -1, "": -1,
	}
	for c, e := range tests {
		assert.Equal(t, e, Exponent(c), c)
	}
	assert.Contains(t, SupportedCurrencies(), "TWD")
}

func TestAddSub(t *testing.T) {
	a := MustNew(100, "TWD")
	b := MustNew(50, "TWD")

	sum, err := a.Add(b)
	require.NoError(t, err)
	assert.Equal(t, MustNew(150, "TWD"), sum)

	diff, err := a.Sub(b)
	require.NoError(t, err)
	assert.Equal(t, MustNew(50, "TWD"), diff)

	_, err = b.Sub(a)
	require.ErrorIs(t, err, ErrNegativeAmount)

	_, err = a.Add(MustNew(1, "USD"))
	require.ErrorIs(t, err, ErrCurrencyMismatch)
	_, err = a.Sub(MustNew(1, "USD"))
	require.ErrorIs(t, err, ErrCurrencyMismatch)

	_, err = MustNew(math.MaxInt64, "TWD").Add(MustNew(1, "TWD"))
	require.ErrorIs(t, err, ErrOverflow)
}

func TestMulBps(t *testing.T) {
	tests := []struct {
		name   string
		amount int64
		bps    int64
		want   int64
	}{
		{"2.9% of 1000", 1000, 290, 29},
		{"half-up .5", 10, 500, 1},   // 10 * 5% = 0.5 -> 1
		{"below half", 10, 490, 0},   // 0.49 -> 0
		{"above half", 10, 510, 1},   // 0.51 -> 1
		{"exact", 100000, 290, 2900}, // 2.9% of 100000
		{"zero amount", 0, 290, 0},
		{"zero bps", 100, 0, 0},
		{"100%", 12345, 10000, 12345},
		{"1 bps of 1", 1, 1, 0}, // 0.0001 -> 0
		{"1.5 rounds up", 3, 5000, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MustNew(tt.amount, "TWD").MulBps(tt.bps)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.AmountMinor)
			assert.Equal(t, "TWD", got.Currency)
		})
	}
	_, err := MustNew(math.MaxInt64, "TWD").MulBps(2)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = MustNew(1, "TWD").MulBps(-1)
	require.ErrorIs(t, err, ErrNegativeAmount)
}

func TestCmpEqualZero(t *testing.T) {
	a := MustNew(100, "TWD")
	b := MustNew(200, "TWD")
	c, err := a.Cmp(b)
	require.NoError(t, err)
	assert.Equal(t, -1, c)
	c, err = b.Cmp(a)
	require.NoError(t, err)
	assert.Equal(t, 1, c)
	c, err = a.Cmp(MustNew(100, "TWD"))
	require.NoError(t, err)
	assert.Equal(t, 0, c)
	_, err = a.Cmp(MustNew(1, "USD"))
	require.ErrorIs(t, err, ErrCurrencyMismatch)

	assert.True(t, b.GreaterThan(a))
	assert.False(t, a.GreaterThan(b))
	assert.False(t, a.GreaterThan(MustNew(1, "USD")))
	assert.True(t, a.Equal(MustNew(100, "TWD")))
	assert.False(t, a.Equal(MustNew(100, "USD")))
	assert.True(t, Zero("TWD").IsZero())
	assert.False(t, a.IsZero())
	assert.True(t, a.IsPositive())
	assert.Equal(t, "100 TWD", a.String())
}

func TestProtoRoundTrip(t *testing.T) {
	m := MustNew(4990, "USD")
	p := m.ToProto()
	assert.Equal(t, int64(4990), p.GetAmountMinor())
	assert.Equal(t, "USD", p.GetCurrency())
	back, err := FromProto(p)
	require.NoError(t, err)
	assert.Equal(t, m, back)

	_, err = FromProto(nil)
	require.ErrorIs(t, err, ErrInvalidCurrency)
	_, err = FromProto(&commonv1.Money{AmountMinor: -5, Currency: "USD"})
	require.ErrorIs(t, err, ErrNegativeAmount)
}

func TestMustNewPanics(t *testing.T) {
	assert.Panics(t, func() { MustNew(1, "bad") })
}

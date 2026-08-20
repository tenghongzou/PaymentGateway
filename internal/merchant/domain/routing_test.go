package domain

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoutingPreferencesValidate(t *testing.T) {
	providers := NewProviderSet([]string{"mock", "Stripe "})
	p := &RoutingPreferences{
		MerchantID: uuid.New(),
		Rules: []RoutingRule{
			{Priority: 20, Provider: "STRIPE", Currencies: []string{"usd"}, Countries: []string{"us"}, PaymentMethodTypes: []string{"Card"}, CardBrands: []string{"VISA"}, Enabled: true},
			{Priority: 10, Provider: "mock", Currencies: []string{"TWD"}, Enabled: true},
		},
		FallbackProviders: []string{"Mock"},
	}
	require.NoError(t, p.Validate(providers))
	assert.Equal(t, int32(10), p.Rules[0].Priority, "依 priority 排序")
	assert.Equal(t, "stripe", p.Rules[1].Provider)
	assert.Equal(t, []string{"USD"}, p.Rules[1].Currencies)
	assert.Equal(t, []string{"US"}, p.Rules[1].Countries)
	assert.Equal(t, []string{"card"}, p.Rules[1].PaymentMethodTypes)
	assert.Equal(t, []string{"visa"}, p.Rules[1].CardBrands)
	assert.Equal(t, []string{"mock"}, p.FallbackProviders)
	assert.Equal(t, DefaultMaxAttempts, p.MaxAttempts)

	tests := []struct {
		name string
		p    RoutingPreferences
	}{
		{"dup priority", RoutingPreferences{Rules: []RoutingRule{{Priority: 1, Provider: "mock"}, {Priority: 1, Provider: "stripe"}}}},
		{"unknown provider", RoutingPreferences{Rules: []RoutingRule{{Priority: 1, Provider: "adyen"}}}},
		{"bad currency", RoutingPreferences{Rules: []RoutingRule{{Priority: 1, Provider: "mock", Currencies: []string{"TWDD"}}}}},
		{"bad country", RoutingPreferences{Rules: []RoutingRule{{Priority: 1, Provider: "mock", Countries: []string{"TWN"}}}}},
		{"bad method", RoutingPreferences{Rules: []RoutingRule{{Priority: 1, Provider: "mock", PaymentMethodTypes: []string{"crypto"}}}}},
		{"bad brand", RoutingPreferences{Rules: []RoutingRule{{Priority: 1, Provider: "mock", CardBrands: []string{"diners"}}}}},
		{"bad fallback", RoutingPreferences{FallbackProviders: []string{"adyen"}}},
		{"max attempts too high", RoutingPreferences{MaxAttempts: 6}},
		{"max attempts negative", RoutingPreferences{MaxAttempts: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.p.Validate(providers), ErrParameterInvalid)
		})
	}

	empty := &RoutingPreferences{Rules: []RoutingRule{{Priority: 1}}}
	require.ErrorIs(t, empty.Validate(providers), ErrParameterMissing)

	// 空 ProviderSet 不檢查 provider
	q := &RoutingPreferences{Rules: []RoutingRule{{Priority: 1, Provider: "anything"}}}
	require.NoError(t, q.Validate(nil))

	d := DefaultRoutingPreferences(uuid.New())
	assert.True(t, d.FailoverEnabled)
	assert.Equal(t, 3, d.MaxAttempts)
	assert.Empty(t, d.Rules)
	assert.Nil(t, d.UpdatedAt)
}

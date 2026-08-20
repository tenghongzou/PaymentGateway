package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validParams() NewMerchantParams {
	return NewMerchantParams{Name: "Acme", LegalName: "Acme Co., Ltd.", Country: "tw", DefaultCurrency: "twd", ContactEmail: "ops@acme.example", StatementDescriptor: "ACME*SHOP", ExternalRef: "crm-1"}
}

func TestNewMerchant(t *testing.T) {
	now := time.Now()
	m, err := NewMerchant(validParams(), now)
	require.NoError(t, err)
	assert.Equal(t, StatusActive, m.Status)
	assert.Equal(t, "TW", m.Country)
	assert.Equal(t, "TWD", m.DefaultCurrency)
	assert.True(t, strings.HasPrefix(m.PublicID(), "mch_"))
	id, err := ParseMerchantID(m.PublicID())
	require.NoError(t, err)
	assert.Equal(t, m.ID, id)
	assert.Equal(t, 3, m.Settings.EffectiveMaxAttempts())
	assert.True(t, m.Settings.EffectiveAllowFailover())

	tests := []struct {
		name string
		mut  func(p *NewMerchantParams)
	}{
		{"empty name", func(p *NewMerchantParams) { p.Name = " " }},
		{"long name", func(p *NewMerchantParams) { p.Name = strings.Repeat("x", 101) }},
		{"empty legal", func(p *NewMerchantParams) { p.LegalName = "" }},
		{"bad country", func(p *NewMerchantParams) { p.Country = "TWN" }},
		{"bad currency", func(p *NewMerchantParams) { p.DefaultCurrency = "NT$" }},
		{"bad email", func(p *NewMerchantParams) { p.ContactEmail = "nope" }},
		{"email with name", func(p *NewMerchantParams) { p.ContactEmail = "Ops <ops@acme.example>" }},
		{"long descriptor", func(p *NewMerchantParams) { p.StatementDescriptor = strings.Repeat("A", 23) }},
		{"non ascii descriptor", func(p *NewMerchantParams) { p.StatementDescriptor = "商店" }},
		{"reserved metadata", func(p *NewMerchantParams) { p.Metadata = map[string]string{"_x": "1"} }},
		{"long metadata value", func(p *NewMerchantParams) { p.Metadata = map[string]string{"k": strings.Repeat("v", 501)} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validParams()
			tt.mut(&p)
			_, err := NewMerchant(p, now)
			require.Error(t, err)
		})
	}
}

func TestParseMerchantID(t *testing.T) {
	_, err := ParseMerchantID("")
	require.ErrorIs(t, err, ErrParameterMissing)
	_, err = ParseMerchantID("pay_01J5X1Y2Z3A4B5C6D7E8F9G0H1")
	require.ErrorIs(t, err, ErrParameterInvalid)
	_, err = ParseMerchantID("mch_short")
	require.ErrorIs(t, err, ErrParameterInvalid)
}

func TestStatusTransitions(t *testing.T) {
	tests := []struct {
		from, to Status
		ok       bool
	}{
		{StatusActive, StatusSuspended, true},
		{StatusSuspended, StatusActive, true},
		{StatusActive, StatusClosed, true},
		{StatusSuspended, StatusClosed, true},
		{StatusClosed, StatusActive, false},
		{StatusClosed, StatusSuspended, false},
		{StatusPending, StatusActive, true},
		{StatusPending, StatusSuspended, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.ok, tt.from.CanTransitionTo(tt.to), "%s → %s", tt.from, tt.to)
	}
	m := &Merchant{ID: uuid.New(), Status: StatusActive}
	require.NoError(t, m.Transition(StatusActive), "同狀態 no-op")
	require.NoError(t, m.Transition(StatusSuspended))
	require.ErrorIs(t, m.AssertCanCreatePayment(), ErrMerchantSuspended)
	require.NoError(t, m.AssertWritable(), "suspended 仍可改設定")
	require.NoError(t, m.Transition(StatusClosed))
	require.ErrorIs(t, m.Transition(StatusActive), ErrInvalidStateTransition)
	require.ErrorIs(t, m.AssertWritable(), ErrMerchantClosed)
	require.ErrorIs(t, m.AssertCanCreatePayment(), ErrMerchantClosed)

	_, err := ParseStatus("deleted")
	require.Error(t, err)
}

func TestMerchantSetters(t *testing.T) {
	m, err := NewMerchant(validParams(), time.Now())
	require.NoError(t, err)
	require.NoError(t, m.Rename("New"))
	require.Error(t, m.Rename(""))
	require.NoError(t, m.SetDefaultCurrency("usd"))
	assert.Equal(t, "USD", m.DefaultCurrency)
	require.Error(t, m.SetContactEmail("x"))
	require.NoError(t, m.SetStatementDescriptor(""))
	require.NoError(t, m.SetMetadata(map[string]string{"a": "b"}))
	assert.Equal(t, "b", m.Metadata["a"])
	require.NoError(t, m.SetLegalName("L"))
	require.Error(t, m.SetLegalName(" "))
}

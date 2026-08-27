package domain

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChartOfAccounts_TenKinds(t *testing.T) {
	specs := ChartOfAccounts()
	require.Len(t, specs, 10)
	seen := map[Kind]bool{}
	for _, s := range specs {
		assert.False(t, seen[s.Kind], "duplicate kind %s", s.Kind)
		seen[s.Kind] = true
		assert.True(t, s.Type.Valid())
		// type 與 normal_balance 的會計對應（與 accounts_type_normal_balance CHECK 一致）
		switch s.Type {
		case AccountTypeAsset, AccountTypeExpense:
			assert.Equal(t, NormalDebit, s.Type.NormalBalance(), s.Kind)
		case AccountTypeLiability, AccountTypeRevenue:
			assert.Equal(t, NormalCredit, s.Type.NormalBalance(), s.Kind)
		}
	}
	// 商戶層級的三個科目
	for _, k := range []Kind{KindMerchantPayable, KindRefundClearing, KindChargebackReserve} {
		spec, ok := SpecOf(k)
		require.True(t, ok)
		assert.Equal(t, LevelMerchant, spec.Level)
		assert.False(t, spec.Qualified)
	}
	// provider / bank 維度的科目必須帶後綴
	for _, k := range []Kind{KindPSPReceivable, KindBankCash, KindSettlementSuspense, KindPSPFeeExpense, KindChargebackFeeExpense} {
		spec, ok := SpecOf(k)
		require.True(t, ok)
		assert.Equal(t, LevelSystem, spec.Level)
		assert.True(t, spec.Qualified)
	}
}

func TestParseCode(t *testing.T) {
	tests := []struct {
		code      string
		wantKind  Kind
		wantQual  string
		wantError bool
	}{
		{"merchant_payable", KindMerchantPayable, "", false},
		{"psp_receivable:stripe", KindPSPReceivable, "stripe", false},
		{"bank_cash:ctbc_001", KindBankCash, "ctbc_001", false},
		{"chargeback_fee_expense:mock", KindChargebackFeeExpense, "mock", false},
		{"psp_receivable", "", "", true},          // 缺後綴
		{"merchant_payable:stripe", "", "", true}, // 不該有後綴
		{"psp_receivable:Stripe", "", "", true},   // 大寫不允許
		{"unknown_code", "", "", true},
		{"", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			kind, q, err := ParseCode(tt.code)
			if tt.wantError {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalidAccountCode)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKind, kind)
			assert.Equal(t, tt.wantQual, q)
		})
	}
}

func TestCodeFor_RoundTrip(t *testing.T) {
	for _, spec := range ChartOfAccounts() {
		q := ""
		if spec.Qualified {
			q = "stripe"
		}
		code, err := CodeFor(spec.Kind, q)
		require.NoError(t, err)
		kind, gotQ, err := ParseCode(code)
		require.NoError(t, err)
		assert.Equal(t, spec.Kind, kind)
		assert.Equal(t, q, gotQ)
	}
	_, err := CodeFor(KindPSPReceivable, "")
	require.Error(t, err)
	_, err = CodeFor(KindFeeRevenue, "x")
	require.Error(t, err)
}

func TestStorageCode_Livemode(t *testing.T) {
	assert.Equal(t, "merchant_payable", StorageCode("merchant_payable", true))
	assert.Equal(t, "test:merchant_payable", StorageCode("merchant_payable", false))
	code, live := ParseStorageCode("test:psp_receivable:stripe")
	assert.Equal(t, "psp_receivable:stripe", code)
	assert.False(t, live)
	code, live = ParseStorageCode("fee_revenue")
	assert.Equal(t, "fee_revenue", code)
	assert.True(t, live)
}

func TestAccountKey_Validate(t *testing.T) {
	m := uuid.New()
	tests := []struct {
		name    string
		key     AccountKey
		wantErr error
	}{
		{"system ok", PSPReceivable("stripe", "TWD", true), nil},
		{"merchant ok", MerchantPayable(m, "USD", true), nil},
		{"system with merchant", AccountKey{MerchantID: m, Code: "fee_revenue", Currency: "TWD", Livemode: true}, ErrInvalidAccountCode},
		{"merchant without merchant", AccountKey{Code: "merchant_payable", Currency: "TWD", Livemode: true}, ErrInvalidAccountCode},
		{"bad currency", MerchantPayable(m, "XXX", true), ErrInvalidCurrency},
		{"lowercase currency", MerchantPayable(m, "twd", true), ErrInvalidCurrency},
		{"bad code", AccountKey{Code: "nope", Currency: "TWD", Livemode: true}, ErrInvalidAccountCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.key.Validate()
			if tt.wantErr == nil {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestNewAccount(t *testing.T) {
	m := uuid.New()
	a, err := NewAccount(ChargebackReserve(m, "TWD", false))
	require.NoError(t, err)
	assert.Equal(t, AccountTypeLiability, a.Type)
	assert.Equal(t, NormalCredit, a.NormalBalance)
	assert.Equal(t, AccountActive, a.Status)
	assert.True(t, a.CanPost())
	assert.Contains(t, a.Name, "(test)")
	assert.Equal(t, KindChargebackReserve, a.Kind())

	// 借貸對餘額的帶號影響
	assert.Equal(t, int64(100), a.SignedDelta(Credit, 100))
	assert.Equal(t, int64(-100), a.SignedDelta(Debit, 100))

	asset, err := NewAccount(PSPReceivable("mock", "TWD", true))
	require.NoError(t, err)
	assert.Equal(t, int64(100), asset.SignedDelta(Debit, 100))
	assert.Equal(t, int64(-100), asset.SignedDelta(Credit, 100))

	_, err = NewAccount(AccountKey{Code: "merchant_payable", Currency: "TWD", Livemode: true})
	require.Error(t, err)
}

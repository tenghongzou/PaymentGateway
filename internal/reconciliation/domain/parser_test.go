package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mockHeader = "type,provider_reference,amount_minor,currency,fee_minor,settled_at\n"

func TestMockParser_Fixture(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", "test", "fixtures", "settlement", "mock-2026-08-19.csv"))
	require.NoError(t, err)
	defer f.Close()

	lines, err := NewMockParser().Parse(f)
	require.NoError(t, err)
	require.Len(t, lines, 6)

	assert.Equal(t, 1, lines[0].LineNo)
	assert.Equal(t, LinePayment, lines[0].Type)
	assert.Equal(t, "mock_ch_01K2W3X4Y5Z6A7B8C9D0E1F2G3", lines[0].ProviderReference)
	assert.Equal(t, int64(1000), lines[0].Amount.AmountMinor)
	assert.Equal(t, "TWD", lines[0].Amount.Currency)
	assert.Equal(t, int64(59), lines[0].Fee.AmountMinor)
	assert.Equal(t, "TWD", lines[0].Fee.Currency)
	assert.Equal(t, "2026-08-19T02:15:00Z", lines[0].SettledAt.Format("2006-01-02T15:04:05Z07:00"))
	assert.Equal(t, MockProvider, lines[0].Provider)
	assert.Equal(t, "59", lines[0].Raw["fee_minor"])

	assert.Equal(t, LineRefund, lines[3].Type)
	assert.Equal(t, LineChargeback, lines[4].Type)
	assert.Equal(t, LineFee, lines[5].Type)
	assert.Equal(t, int64(941), lines[0].Net().AmountMinor)
}

func TestMockParser_Edges(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantErr   error // errors.Is 目標；nil 表示成功
		wantLine  int   // ParseError.Line（0 表示不檢查）
		wantField string
		wantCount int
	}{
		{name: "empty file", input: "", wantErr: ErrEmptyFile},
		{name: "header only", input: mockHeader, wantErr: ErrEmptyFile},
		{name: "header only with blank lines", input: mockHeader + "\n\n", wantErr: ErrEmptyFile},
		{name: "missing column", input: "type,provider_reference,amount_minor\npayment,x,1\n", wantErr: ErrParse},
		{name: "bad type", input: mockHeader + "payout,ref1,100,TWD,0,2026-08-19T00:00:00Z\n", wantErr: ErrParse, wantLine: 1, wantField: "type"},
		{name: "charge alias", input: mockHeader + "charge,ref1,100,TWD,0,2026-08-19T00:00:00Z\n", wantCount: 1},
		{name: "decimal amount rejected", input: mockHeader + "payment,ref1,10.50,TWD,0,2026-08-19T00:00:00Z\n", wantErr: ErrParse, wantLine: 1, wantField: "amount_minor"},
		{name: "negative amount rejected", input: mockHeader + "payment,ref1,-5,TWD,0,2026-08-19T00:00:00Z\n", wantErr: ErrParse, wantLine: 1, wantField: "amount_minor"},
		{name: "empty reference", input: mockHeader + "payment,,100,TWD,0,2026-08-19T00:00:00Z\n", wantErr: ErrParse, wantLine: 1, wantField: "provider_reference"},
		{name: "unsupported currency", input: mockHeader + "payment,ref1,100,XXX,0,2026-08-19T00:00:00Z\n", wantErr: ErrParse, wantLine: 1, wantField: "currency"},
		{name: "lowercase currency normalised", input: mockHeader + "payment,ref1,100,twd,0,2026-08-19T00:00:00Z\n", wantCount: 1},
		{name: "bad timestamp", input: mockHeader + "payment,ref1,100,TWD,0,2026/08/19\n", wantErr: ErrParse, wantLine: 1, wantField: "settled_at"},
		{name: "too few columns", input: mockHeader + "payment,ref1,100\n", wantErr: ErrParse, wantLine: 1},
		{name: "second row bad reports line 2", input: mockHeader + "payment,ref1,100,TWD,0,2026-08-19T00:00:00Z\npayment,ref2,abc,TWD,0,2026-08-19T00:00:00Z\n", wantErr: ErrParse, wantLine: 2, wantField: "amount_minor"},
		{name: "empty fee allowed", input: mockHeader + "payment,ref1,100,TWD,,2026-08-19T00:00:00Z\n", wantCount: 1},
		{name: "blank rows skipped", input: mockHeader + "\npayment,ref1,100,TWD,0,2026-08-19T00:00:00Z\n\n", wantCount: 1},
		{name: "crlf", input: strings.ReplaceAll(mockHeader+"payment,ref1,100,TWD,0,2026-08-19T00:00:00Z\n", "\n", "\r\n"), wantCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, err := NewMockParser().Parse(strings.NewReader(tt.input))
			if tt.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tt.wantErr, "got %v", err)
				if tt.wantLine > 0 {
					var pe *ParseError
					require.ErrorAs(t, err, &pe, "expected *ParseError, got %v", err)
					assert.Equal(t, tt.wantLine, pe.Line)
					if tt.wantField != "" {
						assert.Equal(t, tt.wantField, pe.Field)
					}
				}
				return
			}
			require.NoError(t, err)
			assert.Len(t, lines, tt.wantCount)
		})
	}
}

func TestStripeParser_Fixture(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", "test", "fixtures", "settlement", "stripe-balance-sample.csv"))
	require.NoError(t, err)
	defer f.Close()

	lines, err := NewStripeParser().Parse(f)
	require.NoError(t, err)
	// payout 列略過。
	require.Len(t, lines, 4)

	assert.Equal(t, LinePayment, lines[0].Type)
	assert.Equal(t, "ch_3Pabc001", lines[0].ProviderReference)
	assert.Equal(t, int64(1234), lines[0].Amount.AmountMinor)
	assert.Equal(t, "USD", lines[0].Amount.Currency)
	assert.Equal(t, int64(66), lines[0].Fee.AmountMinor)
	assert.Equal(t, "2026-08-19T01:02:03Z", lines[0].SettledAt.Format("2006-01-02T15:04:05Z07:00"))

	assert.Equal(t, LineRefund, lines[1].Type)
	assert.Equal(t, int64(500), lines[1].Amount.AmountMinor, "負數取絕對值")

	assert.Equal(t, LinePayment, lines[2].Type)
	assert.Equal(t, "JPY", lines[2].Amount.Currency)
	assert.Equal(t, int64(1200), lines[2].Amount.AmountMinor, "JPY 無小數")
	assert.Equal(t, int64(36), lines[2].Fee.AmountMinor)

	assert.Equal(t, LineChargeback, lines[3].Type, "adjustment + du_ 視為 chargeback")
	assert.Equal(t, int64(1234), lines[3].Amount.AmountMinor)
	assert.Equal(t, int64(1500), lines[3].Fee.AmountMinor)
}

func TestStripeParser_CurrencyExponent(t *testing.T) {
	const header = "id,type,source,amount,fee,net,currency,created\n"
	row := func(amount, fee, cur string) string {
		return header + "txn_1,charge,ch_1," + amount + "," + fee + ",0," + cur + ",2026-08-19T00:00:00Z\n"
	}
	tests := []struct {
		name       string
		input      string
		wantAmount int64
		wantFee    int64
		wantErr    bool
	}{
		{name: "USD two decimals", input: row("12.34", "0.66", "usd"), wantAmount: 1234, wantFee: 66},
		{name: "USD one decimal padded", input: row("12.3", "0.5", "USD"), wantAmount: 1230, wantFee: 50},
		{name: "USD no decimals", input: row("12", "1", "USD"), wantAmount: 1200, wantFee: 100},
		{name: "USD three decimals rejected", input: row("12.345", "0", "USD"), wantErr: true},
		{name: "JPY integer", input: row("1200", "36", "JPY"), wantAmount: 1200, wantFee: 36},
		{name: "JPY decimal rejected", input: row("10.5", "0", "JPY"), wantErr: true},
		{name: "KWD three decimals", input: row("1.234", "0.001", "KWD"), wantAmount: 1234, wantFee: 1},
		{name: "thousands separator", input: row("\"1,234.56\"", "0", "USD"), wantAmount: 123456},
		{name: "leading dot", input: row(".50", "0", "USD"), wantAmount: 50},
		{name: "garbage", input: row("12.3a", "0", "USD"), wantErr: true},
		{name: "unsupported currency", input: row("1", "0", "ABC"), wantErr: true},
		{name: "empty amount", input: row("", "0", "USD"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, err := NewStripeParser().Parse(strings.NewReader(tt.input))
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrParse, "got %v", err)
				return
			}
			require.NoError(t, err)
			require.Len(t, lines, 1)
			assert.Equal(t, tt.wantAmount, lines[0].Amount.AmountMinor)
			assert.Equal(t, tt.wantFee, lines[0].Fee.AmountMinor)
		})
	}
}

func TestStripeParser_Edges(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		_, err := NewStripeParser().Parse(strings.NewReader(""))
		assert.ErrorIs(t, err, ErrEmptyFile)
	})
	t.Run("only payouts", func(t *testing.T) {
		_, err := NewStripeParser().Parse(strings.NewReader("id,type,source,amount,fee,currency,created\ntxn_1,payout,po_1,-1.00,0,usd,2026-08-19\n"))
		assert.ErrorIs(t, err, ErrEmptyFile)
	})
	t.Run("missing created column", func(t *testing.T) {
		_, err := NewStripeParser().Parse(strings.NewReader("id,type,source,amount,fee,currency\ntxn_1,charge,ch_1,1.00,0,usd\n"))
		assert.ErrorIs(t, err, ErrParse)
	})
	t.Run("falls back to id when source empty", func(t *testing.T) {
		lines, err := NewStripeParser().Parse(strings.NewReader("id,type,source,amount,fee,currency,created\ntxn_9,charge,,1.00,0,usd,2026-08-19\n"))
		require.NoError(t, err)
		assert.Equal(t, "txn_9", lines[0].ProviderReference)
	})
}

func TestParserFor(t *testing.T) {
	p, err := ParserFor(FormatMockCSV)
	require.NoError(t, err)
	assert.Equal(t, MockProvider, p.Provider())

	p, err = ParserFor(FormatStripeBalanceCSV)
	require.NoError(t, err)
	assert.Equal(t, StripeProvider, p.Provider())

	_, err = ParserFor("adyen_xml")
	assert.ErrorIs(t, err, ErrUnknownFormat)
}

func TestFileHash(t *testing.T) {
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", FileHash(nil))
	assert.Len(t, FileHash([]byte("abc")), 64)
}

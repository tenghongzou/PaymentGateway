package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

var (
	now        = time.Date(2026, 8, 20, 6, 0, 0, 0, time.UTC)
	merchantID = uuid.MustParse("0190f0a0-0000-7000-8000-000000000001")
)

func line(t LineType, ref string, amount, fee int64, cur string) SettlementLine {
	return SettlementLine{
		ID: uuid.New(), LineNo: 1, Provider: MockProvider, ProviderReference: ref, Type: t,
		Amount:    money.Money{AmountMinor: amount, Currency: cur},
		Fee:       money.Money{AmountMinor: fee, Currency: cur},
		SettledAt: now.Add(-24 * time.Hour),
	}
}

func record(kind RecordKind, ref, status string, amount int64, cur string) PaymentRecord {
	return PaymentRecord{
		ID: uuid.New(), Kind: kind, PublicID: string(kind) + "_" + ref, MerchantID: merchantID,
		Provider: MockProvider, ProviderReference: ref, Status: status,
		Amount:     money.Money{AmountMinor: amount, Currency: cur},
		OccurredAt: now.Add(-48 * time.Hour), SourceSeq: 1,
	}
}

func withFee(r PaymentRecord, fee int64) PaymentRecord {
	f := money.Money{AmountMinor: fee, Currency: r.Amount.Currency}
	r.Fee = &f
	return r
}

func TestMatcher_Kinds(t *testing.T) {
	tests := []struct {
		name         string
		lines        []SettlementLine
		records      []PaymentRecord
		grace        time.Duration
		wantMatched  int
		wantKinds    []DiscrepancyKind
		wantSkipped  int
		wantDeferred int
		check        func(t *testing.T, res MatchResult)
	}{
		{
			name:        "exact match payment",
			lines:       []SettlementLine{line(LinePayment, "ch_1", 1000, 59, "TWD")},
			records:     []PaymentRecord{withFee(record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD"), 59)},
			wantMatched: 1,
		},
		{
			name:        "match ignores fee when internal fee unknown",
			lines:       []SettlementLine{line(LinePayment, "ch_1", 1000, 59, "TWD")},
			records:     []PaymentRecord{record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")},
			wantMatched: 1,
		},
		{
			name:      "missing_in_ledger",
			lines:     []SettlementLine{line(LinePayment, "ch_404", 1000, 0, "TWD")},
			wantKinds: []DiscrepancyKind{KindMissingInLedger},
			check: func(t *testing.T, res MatchResult) {
				d := res.Discrepancies[0]
				assert.Equal(t, "ch_404", d.ProviderReference)
				assert.Nil(t, d.ExpectedAmount)
				assert.Equal(t, int64(1000), *d.ActualAmount)
				assert.NotNil(t, d.SettlementLineID)
				assert.Nil(t, d.MerchantID)
				snap, ok := d.LineSnapshot()
				require.True(t, ok)
				assert.Equal(t, int64(1000), snap.Amount.AmountMinor)
				assert.Equal(t, LinePayment, snap.Type)
			},
		},
		{
			name:      "missing_in_ledger when type differs",
			lines:     []SettlementLine{line(LineRefund, "ch_1", 1000, 0, "TWD")},
			records:   []PaymentRecord{record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")},
			wantKinds: []DiscrepancyKind{KindMissingInLedger, KindMissingInPSP},
		},
		{
			name:      "missing_in_psp",
			records:   []PaymentRecord{record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")},
			wantKinds: []DiscrepancyKind{KindMissingInPSP},
			check: func(t *testing.T, res MatchResult) {
				d := res.Discrepancies[0]
				assert.Equal(t, "payment_ch_1", d.InternalReference)
				assert.Equal(t, int64(1000), *d.ExpectedAmount)
				assert.Nil(t, d.ActualAmount)
				require.NotNil(t, d.MerchantID)
				assert.Equal(t, merchantID, *d.MerchantID)
				assert.Equal(t, StatusCaptured, d.Detail(DetailExpectedStatus))
			},
		},
		{
			name:         "missing_in_psp deferred within grace",
			records:      []PaymentRecord{record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")},
			grace:        72 * time.Hour,
			wantDeferred: 1,
		},
		{
			name:      "missing_in_psp opened after grace",
			records:   []PaymentRecord{record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")},
			grace:     24 * time.Hour,
			wantKinds: []DiscrepancyKind{KindMissingInPSP},
		},
		{
			name: "non-settleable records never missing_in_psp",
			records: []PaymentRecord{
				record(RecordPayment, "ch_a", StatusAuthorized, 1000, "TWD"),
				record(RecordPayment, "ch_v", StatusVoided, 1000, "TWD"),
				record(RecordRefund, "re_p", RefundPending, 100, "TWD"),
				record(RecordDispute, "du_w", DisputeWon, 100, "TWD"),
			},
		},
		{
			name:      "amount_mismatch",
			lines:     []SettlementLine{line(LinePayment, "ch_1", 900, 0, "TWD")},
			records:   []PaymentRecord{record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")},
			wantKinds: []DiscrepancyKind{KindAmountMismatch},
			check: func(t *testing.T, res MatchResult) {
				d := res.Discrepancies[0]
				assert.Equal(t, int64(1000), *d.ExpectedAmount)
				assert.Equal(t, int64(900), *d.ActualAmount)
				assert.Equal(t, "TWD", d.Currency)
				assert.Equal(t, "payment_ch_1", d.InternalReference)
				assert.Empty(t, d.Detail(DetailReason))
			},
		},
		{
			name:      "currency mismatch reported as amount_mismatch",
			lines:     []SettlementLine{line(LinePayment, "ch_1", 1000, 0, "USD")},
			records:   []PaymentRecord{record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")},
			wantKinds: []DiscrepancyKind{KindAmountMismatch},
			check: func(t *testing.T, res MatchResult) {
				assert.Equal(t, "currency_mismatch", res.Discrepancies[0].Detail(DetailReason))
				assert.Equal(t, "TWD", res.Discrepancies[0].Detail("expected_currency"))
			},
		},
		{
			name:      "status_mismatch voided payment settled",
			lines:     []SettlementLine{line(LinePayment, "ch_1", 1000, 0, "TWD")},
			records:   []PaymentRecord{record(RecordPayment, "ch_1", StatusVoided, 1000, "TWD")},
			wantKinds: []DiscrepancyKind{KindStatusMismatch},
			check: func(t *testing.T, res MatchResult) {
				d := res.Discrepancies[0]
				assert.Equal(t, StatusVoided, d.Detail(DetailExpectedStatus))
				assert.Equal(t, "settled", d.Detail(DetailActualStatus))
			},
		},
		{
			name:      "status_mismatch pending refund settled",
			lines:     []SettlementLine{line(LineRefund, "re_1", 300, 0, "TWD")},
			records:   []PaymentRecord{record(RecordRefund, "re_1", RefundPending, 300, "TWD")},
			wantKinds: []DiscrepancyKind{KindStatusMismatch},
		},
		{
			name:      "status_mismatch won dispute settled as chargeback",
			lines:     []SettlementLine{line(LineChargeback, "du_1", 1000, 0, "TWD")},
			records:   []PaymentRecord{record(RecordDispute, "du_1", DisputeWon, 1000, "TWD")},
			wantKinds: []DiscrepancyKind{KindStatusMismatch},
		},
		{
			name:      "status takes precedence over amount",
			lines:     []SettlementLine{line(LinePayment, "ch_1", 1, 0, "TWD")},
			records:   []PaymentRecord{record(RecordPayment, "ch_1", StatusAuthorized, 1000, "TWD")},
			wantKinds: []DiscrepancyKind{KindStatusMismatch},
		},
		{
			name:      "fee_mismatch",
			lines:     []SettlementLine{line(LinePayment, "ch_1", 1000, 60, "TWD")},
			records:   []PaymentRecord{withFee(record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD"), 59)},
			wantKinds: []DiscrepancyKind{KindFeeMismatch},
			check: func(t *testing.T, res MatchResult) {
				d := res.Discrepancies[0]
				assert.Equal(t, int64(59), *d.ExpectedAmount)
				assert.Equal(t, int64(60), *d.ActualAmount)
				assert.EqualValues(t, 59, d.Details[DetailExpectedFee])
				assert.EqualValues(t, 60, d.Details[DetailActualFee])
			},
		},
		{
			name:      "amount takes precedence over fee",
			lines:     []SettlementLine{line(LinePayment, "ch_1", 999, 60, "TWD")},
			records:   []PaymentRecord{withFee(record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD"), 59)},
			wantKinds: []DiscrepancyKind{KindAmountMismatch},
		},
		{
			name:        "refund and chargeback match",
			lines:       []SettlementLine{line(LineRefund, "re_1", 300, 0, "TWD"), line(LineChargeback, "du_1", 1000, 450, "TWD")},
			records:     []PaymentRecord{record(RecordRefund, "re_1", RefundSucceeded, 300, "TWD"), record(RecordDispute, "du_1", DisputeLost, 1000, "TWD")},
			wantMatched: 2,
		},
		{
			name:        "fee and adjustment lines skipped",
			lines:       []SettlementLine{line(LineFee, "fee_1", 1500, 0, "TWD"), line(LineAdjustment, "adj_1", 10, 0, "TWD")},
			wantSkipped: 2,
		},
		{
			name:        "duplicate in settlement",
			lines:       []SettlementLine{line(LinePayment, "ch_1", 1000, 0, "TWD"), line(LinePayment, "ch_1", 1000, 0, "TWD")},
			records:     []PaymentRecord{record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")},
			wantMatched: 1,
			wantKinds:   []DiscrepancyKind{KindMissingInLedger},
			check: func(t *testing.T, res MatchResult) {
				assert.Equal(t, "duplicate_in_settlement", res.Discrepancies[0].Detail(DetailReason))
			},
		},
		{
			name:  "records from other provider ignored",
			lines: []SettlementLine{line(LinePayment, "ch_1", 1000, 0, "TWD")},
			records: []PaymentRecord{func() PaymentRecord {
				r := record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")
				r.Provider = "stripe"
				return r
			}()},
			wantKinds:   []DiscrepancyKind{KindMissingInLedger, KindMissingInPSP},
			wantMatched: 0,
		},
		{
			name:  "latest source_seq wins for duplicate records",
			lines: []SettlementLine{line(LinePayment, "ch_1", 1000, 0, "TWD")},
			records: []PaymentRecord{
				func() PaymentRecord {
					r := record(RecordPayment, "ch_1", StatusAuthorized, 1000, "TWD")
					r.SourceSeq = 1
					return r
				}(),
				func() PaymentRecord {
					r := record(RecordPayment, "ch_1", StatusCaptured, 1000, "TWD")
					r.SourceSeq = 2
					return r
				}(),
			},
			wantMatched: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := NewMatcher().Match(MatchInput{Provider: MockProvider, Lines: tt.lines, Records: tt.records, Now: now, GracePeriod: tt.grace})
			assert.Equal(t, len(tt.lines), res.TotalLines)
			assert.Len(t, res.Matched, tt.wantMatched, "matched")
			assert.Equal(t, tt.wantSkipped, res.Skipped, "skipped")
			assert.Equal(t, tt.wantDeferred, res.Deferred, "deferred")
			var kinds []DiscrepancyKind
			for _, d := range res.Discrepancies {
				kinds = append(kinds, d.Kind)
				assert.Equal(t, DiscrepancyOpen, d.Status)
				assert.NotEqual(t, uuid.Nil, d.ID)
				assert.NotEmpty(t, d.Details["description"])
				assert.True(t, d.Kind.IsValid())
			}
			assert.ElementsMatch(t, tt.wantKinds, kinds)
			if tt.check != nil {
				tt.check(t, res)
			}
		})
	}
}

func TestMatcher_Totals(t *testing.T) {
	res := NewMatcher().Match(MatchInput{Provider: MockProvider, Now: now, Lines: []SettlementLine{
		line(LinePayment, "ch_1", 1000, 59, "TWD"),
		line(LinePayment, "ch_2", 1999, 88, "USD"),
		line(LineRefund, "re_1", 300, 0, "TWD"),
		line(LineChargeback, "du_1", 1000, 450, "TWD"),
		line(LineFee, "fee_1", 1500, 0, "TWD"),
	}})
	assert.Equal(t, int64(1000), res.Totals.Settled["TWD"])
	assert.Equal(t, int64(1999), res.Totals.Settled["USD"])
	assert.Equal(t, int64(59+450+1500), res.Totals.Fees["TWD"])
	assert.Equal(t, int64(88), res.Totals.Fees["USD"])
	assert.Equal(t, int64(300), res.Totals.Refunds["TWD"])
	assert.Equal(t, int64(1000), res.Totals.Chargebacks["TWD"])
	assert.Equal(t, 1, res.Skipped)
	assert.Len(t, res.Discrepancies, 4, "四條可比對的列都沒有內部紀錄")
}

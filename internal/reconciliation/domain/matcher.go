package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// MatchInput 為一次比對的輸入。
type MatchInput struct {
	Provider string
	Lines    []SettlementLine
	// Records 為候選內部紀錄：依 Lines 的 provider_reference 查到的紀錄 + 尚未被任何結算列對上的「應結算」紀錄。
	Records []PaymentRecord
	// Now 與 GracePeriod 用於 missing_in_psp：occurred_at 距 Now 未超過 GracePeriod 的紀錄暫不開單（PSP 延遲結算）。
	Now         time.Time
	GracePeriod time.Duration
}

// MatchedPair 為對上的結算列與內部紀錄。
type MatchedPair struct {
	Line   SettlementLine
	Record PaymentRecord
}

// Totals 為結算檔各幣別合計（最小單位）。
type Totals struct {
	Settled     map[string]int64 // payment 列毛額
	Fees        map[string]int64 // 所有列的手續費 + fee 列金額
	Refunds     map[string]int64
	Chargebacks map[string]int64
}

// MatchResult 為比對結果；Discrepancies 的 RunID 由呼叫端填入。
type MatchResult struct {
	TotalLines    int
	Matched       []MatchedPair
	Discrepancies []Discrepancy
	// Skipped 為不參與比對的列（fee / adjustment）。
	Skipped int
	// Deferred 為 grace 期內暫不開單的內部紀錄數。
	Deferred int
	Totals   Totals
}

// Matcher 以 (provider_reference, type) 比對結算列與內部讀模型。
//
// 每一條結算列最多產生一筆差異，優先序：status_mismatch > amount_mismatch（含幣別）> fee_mismatch。
type Matcher struct{}

// NewMatcher 建立 Matcher。
func NewMatcher() *Matcher { return &Matcher{} }

// Match 執行比對。
func (m *Matcher) Match(in MatchInput) MatchResult {
	res := MatchResult{
		TotalLines: len(in.Lines),
		Totals: Totals{
			Settled: map[string]int64{}, Fees: map[string]int64{}, Refunds: map[string]int64{}, Chargebacks: map[string]int64{},
		},
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	// 內部紀錄索引：只收有 provider_reference 的紀錄；同鍵多筆取 source_seq 最大者。
	records := make(map[MatchKey]PaymentRecord, len(in.Records))
	for _, r := range in.Records {
		if r.ProviderReference == "" || r.Provider != "" && in.Provider != "" && r.Provider != in.Provider {
			continue
		}
		k := r.Key()
		if cur, ok := records[k]; !ok || r.SourceSeq >= cur.SourceSeq {
			records[k] = r
		}
	}
	consumed := map[MatchKey]bool{}

	for _, line := range in.Lines {
		m.accumulate(&res.Totals, line)
		if !line.Type.Matchable() {
			res.Skipped++
			continue
		}
		key := line.Key()
		if consumed[key] {
			// 同一參照在結算檔重複出現：第二筆起視為 PSP 多結算。
			res.Discrepancies = append(res.Discrepancies, newLineDiscrepancy(KindMissingInLedger, in.Provider, line, nil,
				map[string]any{DetailReason: "duplicate_in_settlement"}))
			continue
		}
		rec, ok := records[key]
		if !ok {
			res.Discrepancies = append(res.Discrepancies, newLineDiscrepancy(KindMissingInLedger, in.Provider, line, nil, nil))
			continue
		}
		consumed[key] = true
		if d, ok := m.compare(in.Provider, line, rec); ok {
			res.Discrepancies = append(res.Discrepancies, d)
			continue
		}
		res.Matched = append(res.Matched, MatchedPair{Line: line, Record: rec})
	}

	// 我方有、PSP 沒有：只看「應結算」且超過 grace 的紀錄。
	seen := map[MatchKey]bool{}
	for _, r := range in.Records {
		if !r.IsSettleable() {
			continue
		}
		k := r.Key()
		if consumed[k] || seen[k] {
			continue
		}
		seen[k] = true
		if in.GracePeriod > 0 && r.OccurredAt.Add(in.GracePeriod).After(now) {
			res.Deferred++
			continue
		}
		res.Discrepancies = append(res.Discrepancies, newRecordDiscrepancy(KindMissingInPSP, in.Provider, r))
	}
	return res
}

// compare 比對已對上的結算列與內部紀錄；有差異時回傳 (discrepancy, true)。
func (m *Matcher) compare(provider string, line SettlementLine, rec PaymentRecord) (Discrepancy, bool) {
	if !rec.IsSettleable() {
		return newLineDiscrepancy(KindStatusMismatch, provider, line, &rec, map[string]any{
			DetailExpectedStatus: rec.Status,
			DetailActualStatus:   "settled",
		}), true
	}
	if line.Amount.Currency != rec.Amount.Currency {
		return newLineDiscrepancy(KindAmountMismatch, provider, line, &rec, map[string]any{
			DetailReason: "currency_mismatch",
		}), true
	}
	if line.Amount.AmountMinor != rec.Amount.AmountMinor {
		return newLineDiscrepancy(KindAmountMismatch, provider, line, &rec, nil), true
	}
	if rec.Fee != nil && rec.Fee.Currency == line.Fee.Currency && rec.Fee.AmountMinor != line.Fee.AmountMinor {
		d := newLineDiscrepancy(KindFeeMismatch, provider, line, &rec, map[string]any{
			DetailExpectedFee: rec.Fee.AmountMinor,
			DetailActualFee:   line.Fee.AmountMinor,
		})
		exp, act := rec.Fee.AmountMinor, line.Fee.AmountMinor
		d.ExpectedAmount, d.ActualAmount = &exp, &act
		return d, true
	}
	return Discrepancy{}, false
}

// accumulate 累加各幣別合計。
func (m *Matcher) accumulate(t *Totals, line SettlementLine) {
	cur := line.Amount.Currency
	switch line.Type {
	case LinePayment:
		t.Settled[cur] += line.Amount.AmountMinor
	case LineRefund:
		t.Refunds[cur] += line.Amount.AmountMinor
	case LineChargeback:
		t.Chargebacks[cur] += line.Amount.AmountMinor
	case LineFee:
		t.Fees[cur] += line.Amount.AmountMinor
	}
	if line.Fee.AmountMinor != 0 {
		t.Fees[cur] += line.Fee.AmountMinor
	}
}

// newLineDiscrepancy 建立以結算列為證據的差異；rec 非 nil 時帶入內部參照與預期金額。
func newLineDiscrepancy(kind DiscrepancyKind, provider string, line SettlementLine, rec *PaymentRecord, details map[string]any) Discrepancy {
	actual := line.Amount.AmountMinor
	d := Discrepancy{
		ID:                ids.NewUUID(),
		Kind:              kind,
		Provider:          provider,
		ProviderReference: line.ProviderReference,
		ActualAmount:      &actual,
		Currency:          line.Amount.Currency,
		Status:            DiscrepancyOpen,
		Details:           map[string]any{},
	}
	if line.ID != uuid.Nil {
		id := line.ID
		d.SettlementLineID = &id
	}
	d.setDetail(DetailLine, snapshotOf(line))
	if rec != nil {
		expected := rec.Amount.AmountMinor
		d.ExpectedAmount = &expected
		d.InternalReference = rec.PublicID
		if rec.MerchantID != uuid.Nil {
			mid := rec.MerchantID
			d.MerchantID = &mid
		}
		d.setDetail(DetailRecordKind, string(rec.Kind))
		if rec.Amount.Currency != "" && rec.Amount.Currency != line.Amount.Currency {
			d.setDetail("expected_currency", rec.Amount.Currency)
		}
	}
	for k, v := range details {
		d.setDetail(k, v)
	}
	d.setDetail("description", describe(d))
	return d
}

// newRecordDiscrepancy 建立以內部紀錄為證據的差異（missing_in_psp）。
func newRecordDiscrepancy(kind DiscrepancyKind, provider string, rec PaymentRecord) Discrepancy {
	expected := rec.Amount.AmountMinor
	d := Discrepancy{
		ID:                ids.NewUUID(),
		Kind:              kind,
		Provider:          provider,
		ProviderReference: rec.ProviderReference,
		InternalReference: rec.PublicID,
		ExpectedAmount:    &expected,
		Currency:          rec.Amount.Currency,
		Status:            DiscrepancyOpen,
		Details:           map[string]any{DetailRecordKind: string(rec.Kind), DetailExpectedStatus: rec.Status},
	}
	if rec.MerchantID != uuid.Nil {
		mid := rec.MerchantID
		d.MerchantID = &mid
	}
	d.setDetail("description", describe(d))
	return d
}

// describe 產生人類可讀說明。
func describe(d Discrepancy) string {
	switch d.Kind {
	case KindMissingInLedger:
		if d.Detail(DetailReason) == "duplicate_in_settlement" {
			return fmt.Sprintf("provider reference %s appears more than once in the settlement file", d.ProviderReference)
		}
		return fmt.Sprintf("settlement file contains %s but no internal record was found", d.ProviderReference)
	case KindMissingInPSP:
		return fmt.Sprintf("internal record %s (%s) is settleable but absent from the settlement file", d.InternalReference, d.ProviderReference)
	case KindAmountMismatch:
		if d.Detail(DetailReason) == "currency_mismatch" {
			return fmt.Sprintf("currency differs for %s: internal %s vs settlement %s", d.ProviderReference, d.Detail("expected_currency"), d.Currency)
		}
		return fmt.Sprintf("amount differs for %s: internal %d vs settlement %d %s", d.ProviderReference, deref(d.ExpectedAmount), deref(d.ActualAmount), d.Currency)
	case KindStatusMismatch:
		return fmt.Sprintf("internal record %s is %s but the settlement file shows it as settled", d.InternalReference, d.Detail(DetailExpectedStatus))
	case KindFeeMismatch:
		return fmt.Sprintf("fee differs for %s: internal %d vs settlement %d %s", d.ProviderReference, deref(d.ExpectedAmount), deref(d.ActualAmount), d.Currency)
	}
	return string(d.Kind)
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

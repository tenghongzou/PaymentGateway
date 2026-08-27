package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// DiscrepancyKind 為差異類型。
//
// 注意：migrations/reconciliation/0001 的 discrepancies.kind CHECK 目前只列出前四種，
// fee_mismatch 由 postgres adapter 以 kind=amount_mismatch + details.kind=fee_mismatch 落地（見 adapter/postgres）。
// TODO(migration)：請 migrations 維護者把 'fee_mismatch' 加入 CHECK 後移除該對應。
type DiscrepancyKind string

// 差異類型全集（docs/05 §9.2）。
const (
	// KindMissingInLedger：結算檔有、我方無此交易。
	KindMissingInLedger DiscrepancyKind = "missing_in_ledger"
	// KindMissingInPSP：我方有已請款 / 已退款 / 拒付敗訴紀錄、結算檔無。
	KindMissingInPSP DiscrepancyKind = "missing_in_psp"
	// KindAmountMismatch：金額（或幣別）不符。
	KindAmountMismatch DiscrepancyKind = "amount_mismatch"
	// KindStatusMismatch：我方狀態不允許結算（例：voided、refund pending）但 PSP 已結算。
	KindStatusMismatch DiscrepancyKind = "status_mismatch"
	// KindFeeMismatch：手續費不符。
	KindFeeMismatch DiscrepancyKind = "fee_mismatch"
)

// AllDiscrepancyKinds 回傳全部差異類型。
func AllDiscrepancyKinds() []DiscrepancyKind {
	return []DiscrepancyKind{KindMissingInLedger, KindMissingInPSP, KindAmountMismatch, KindStatusMismatch, KindFeeMismatch}
}

// IsValid 檢查是否為合法類型。
func (k DiscrepancyKind) IsValid() bool {
	for _, v := range AllDiscrepancyKinds() {
		if v == k {
			return true
		}
	}
	return false
}

// DiscrepancyStatus 為差異處理狀態：open → resolved | ignored。
type DiscrepancyStatus string

// 差異狀態全集（discrepancies.status CHECK）。
const (
	DiscrepancyOpen     DiscrepancyStatus = "open"
	DiscrepancyResolved DiscrepancyStatus = "resolved"
	DiscrepancyIgnored  DiscrepancyStatus = "ignored"
)

// PrefixDiscrepancy 為差異對外 ID 前綴（proto 註解：dsc_ + ULID；DB 以 uuid 主鍵儲存，對外以 ids.Format 推導）。
const PrefixDiscrepancy = "dsc"

// PrefixRun 為 run 對外 ID 前綴（reconciliation_runs.public_id：rr_xxx）。
const PrefixRun = "rr"

// details 內的固定欄位名。
const (
	DetailReason         = "reason"          // currency_mismatch / duplicate_in_settlement …
	DetailExpectedStatus = "expected_status" // status_mismatch：我方狀態
	DetailActualStatus   = "actual_status"   // status_mismatch：結算檔語意狀態（settled）
	DetailRecordKind     = "record_kind"     // payment / refund / dispute
	DetailLine           = "line"            // 結算列快照（lineSnapshot）
	DetailIdempotencyKey = "idempotency_key" // ResolveDiscrepancy 的冪等鍵
	DetailKind           = "kind"            // adapter 用：真正的 kind（fee_mismatch 落地時）
	DetailExpectedFee    = "expected_fee"
	DetailActualFee      = "actual_fee"
)

// Discrepancy 為一筆對帳差異聚合根（對齊 discrepancies 表）。
type Discrepancy struct {
	ID                uuid.UUID
	RunID             uuid.UUID
	MerchantID        *uuid.UUID // 無法對應商戶時為 nil
	Kind              DiscrepancyKind
	Provider          string
	ProviderReference string
	InternalReference string // pay_ / re_ / dp_
	SettlementLineID  *uuid.UUID
	ExpectedAmount    *int64 // 我方金額
	ActualAmount      *int64 // 結算檔金額
	Currency          string
	Status            DiscrepancyStatus
	ResolutionNote    string
	ResolvedBy        string
	ResolvedAt        *time.Time
	Details           map[string]any
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Version           int
}

// PublicID 回傳對外 ID（dsc_…）。
func (d *Discrepancy) PublicID() string { return ids.Format(PrefixDiscrepancy, d.ID) }

// ParseDiscrepancyID 把對外 ID（dsc_…）或裸 uuid 解析成 uuid。
func ParseDiscrepancyID(s string) (uuid.UUID, error) {
	if strings.HasPrefix(s, PrefixDiscrepancy+"_") {
		u, err := ids.ParseWithPrefix(s, PrefixDiscrepancy)
		if err != nil {
			return uuid.Nil, ErrDiscrepancyNotFound.WithMessage("Invalid discrepancy id %q.", s)
		}
		return u, nil
	}
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, ErrDiscrepancyNotFound.WithMessage("Invalid discrepancy id %q.", s)
	}
	return u, nil
}

// Detail 取 Details 內的字串值（不存在時回空字串）。
func (d *Discrepancy) Detail(key string) string {
	if d.Details == nil {
		return ""
	}
	if v, ok := d.Details[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// setDetail 寫入 Details。
func (d *Discrepancy) setDetail(key string, v any) {
	if d.Details == nil {
		d.Details = map[string]any{}
	}
	d.Details[key] = v
}

// Resolve 把差異標為 resolved（前置狀態 open）。
func (d *Discrepancy) Resolve(note, by string, now time.Time) error {
	return d.close(DiscrepancyResolved, note, by, now, false)
}

// Ignore 把差異標為 ignored（前置狀態 open；必須附備註）。
func (d *Discrepancy) Ignore(note, by string, now time.Time) error {
	return d.close(DiscrepancyIgnored, note, by, now, true)
}

func (d *Discrepancy) close(to DiscrepancyStatus, note, by string, now time.Time, noteRequired bool) error {
	if strings.TrimSpace(by) == "" {
		return ErrResolvedByRequired
	}
	if noteRequired && strings.TrimSpace(note) == "" {
		return ErrResolutionNoteRequired
	}
	if d.Status != DiscrepancyOpen {
		return ErrInvalidTransition.WithMessage("discrepancy %s is already %s", d.PublicID(), d.Status)
	}
	d.Status = to
	d.ResolutionNote = note
	d.ResolvedBy = by
	d.ResolvedAt = &now
	d.UpdatedAt = now
	d.Version++
	return nil
}

// IsOpen 回傳是否仍待處理。
func (d *Discrepancy) IsOpen() bool { return d.Status == DiscrepancyOpen }

// snapshotOf 把結算列轉成 details 可序列化的 map（jsonb 反序列化後為 map[string]any，故直接用 map）。
func snapshotOf(l SettlementLine) map[string]any {
	return map[string]any{
		"line_no":            l.LineNo,
		"type":               string(l.Type),
		"provider_reference": l.ProviderReference,
		"amount_minor":       l.Amount.AmountMinor,
		"fee_minor":          l.Fee.AmountMinor,
		"currency":           l.Amount.Currency,
		"settled_at":         l.SettledAt.UTC().Format(time.RFC3339),
	}
}

// LineSnapshot 從 details 取回結算列快照；沒有時回 ok=false。
func (d *Discrepancy) LineSnapshot() (line SettlementLine, ok bool) {
	if d.Details == nil {
		return SettlementLine{}, false
	}
	m, ok := d.Details[DetailLine].(map[string]any)
	if !ok {
		return SettlementLine{}, false
	}
	line.LineNo = int(toInt64(m["line_no"]))
	line.Type = LineType(toString(m["type"]))
	line.ProviderReference = toString(m["provider_reference"])
	line.Amount.AmountMinor = toInt64(m["amount_minor"])
	line.Fee.AmountMinor = toInt64(m["fee_minor"])
	line.Amount.Currency = toString(m["currency"])
	line.Fee.Currency = line.Amount.Currency
	if t, err := time.Parse(time.RFC3339, toString(m["settled_at"])); err == nil {
		line.SettledAt = t
	}
	if d.SettlementLineID != nil {
		line.ID = *d.SettlementLineID
	}
	line.Provider = d.Provider
	return line, true
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64: //nolint:forbidigo // jsonb details 反序列化後的數字型別為 float64，此處僅還原為 int64 最小單位，非以浮點數運算金額
		return int64(n)
	case int32:
		return int64(n)
	}
	return 0
}

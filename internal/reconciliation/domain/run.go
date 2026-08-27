package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// RunStatus 為對帳執行狀態（reconciliation_runs.status CHECK）。
type RunStatus string

// 對帳執行狀態全集。
const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
)

// RunSummary 為 run 的摘要（序列化到 reconciliation_runs.summary jsonb）。
type RunSummary struct {
	FileID        string `json:"file_id,omitempty"`
	FileName      string `json:"file_name,omitempty"`
	FileHash      string `json:"file_hash,omitempty"`
	FileFormat    string `json:"file_format,omitempty"`
	FileSizeBytes int64  `json:"file_size_bytes,omitempty"`
	// TotalLines 為結算檔總列數。
	TotalLines int `json:"total_lines"`
	// Matched 為完全對上的筆數；Unmatched 為產生差異的筆數（含 missing_in_psp）。
	Matched   int `json:"matched"`
	Unmatched int `json:"unmatched"`
	// Skipped 為不參與比對的列（fee / adjustment）。
	Skipped int `json:"skipped"`
	// Deferred 為我方有、PSP 無但仍在 grace 期內暫不開單的筆數。
	Deferred int `json:"deferred"`
	// Suppressed 為已有 open 差異而不重複開單的筆數。
	Suppressed int `json:"suppressed"`
	// ByKind 為各差異類型計數。
	ByKind map[string]int `json:"by_kind"`
	// 各幣別合計（最小單位）。
	TotalSettled     map[string]int64 `json:"total_settled"`
	TotalFees        map[string]int64 `json:"total_fees"`
	TotalRefunds     map[string]int64 `json:"total_refunds"`
	TotalChargebacks map[string]int64 `json:"total_chargebacks"`
	DurationMs       int64            `json:"duration_ms"`
}

// Run 為一次對帳執行聚合根（對齊 reconciliation_runs 表）。
type Run struct {
	ID             uuid.UUID
	PublicID       string // rr_…
	Provider       string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	Status         RunStatus
	MatchedCount   int
	UnmatchedCount int
	Summary        RunSummary
	Error          string
	StartedAt      *time.Time
	FinishedAt     *time.Time
	TriggeredBy    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Version        int
}

// NewRun 建立 pending 的 run；period_end 必須晚於 period_start。
func NewRun(provider string, periodStart, periodEnd time.Time, triggeredBy string, now time.Time) (*Run, error) {
	if !periodEnd.After(periodStart) {
		return nil, ErrInvalidPeriod
	}
	if strings.TrimSpace(triggeredBy) == "" {
		triggeredBy = "api"
	}
	u := ids.NewUUID()
	return &Run{
		ID:          u,
		PublicID:    ids.Format(PrefixRun, u),
		Provider:    provider,
		PeriodStart: periodStart.UTC(),
		PeriodEnd:   periodEnd.UTC(),
		Status:      RunPending,
		Summary:     RunSummary{ByKind: map[string]int{}},
		TriggeredBy: triggeredBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// ParseRunID 把對外 ID（rr_…）解析成 uuid。
func ParseRunID(s string) (uuid.UUID, error) {
	u, err := ids.ParseWithPrefix(s, PrefixRun)
	if err != nil {
		return uuid.Nil, ErrRunNotFound.WithMessage("Invalid run id %q.", s)
	}
	return u, nil
}

// Start 標記開始比對。
func (r *Run) Start(now time.Time) error {
	if r.Status != RunPending {
		return ErrInvalidTransition.WithMessage("run %s is %s, cannot start", r.PublicID, r.Status)
	}
	r.Status = RunRunning
	r.StartedAt = &now
	r.UpdatedAt = now
	r.Version++
	return nil
}

// Complete 以比對結果完成 run（允許從 pending 直接完成，方便同步執行）。
func (r *Run) Complete(res MatchResult, now time.Time) error {
	if r.Status != RunRunning && r.Status != RunPending {
		return ErrInvalidTransition.WithMessage("run %s is %s, cannot complete", r.PublicID, r.Status)
	}
	if r.StartedAt == nil {
		r.StartedAt = &now
	}
	r.Status = RunCompleted
	r.MatchedCount = len(res.Matched)
	r.UnmatchedCount = len(res.Discrepancies)
	s := &r.Summary
	s.TotalLines = res.TotalLines
	s.Matched = len(res.Matched)
	s.Unmatched = len(res.Discrepancies)
	s.Skipped = res.Skipped
	s.Deferred = res.Deferred
	s.ByKind = map[string]int{}
	for i := range res.Discrepancies {
		s.ByKind[string(res.Discrepancies[i].Kind)]++
	}
	s.TotalSettled = res.Totals.Settled
	s.TotalFees = res.Totals.Fees
	s.TotalRefunds = res.Totals.Refunds
	s.TotalChargebacks = res.Totals.Chargebacks
	s.DurationMs = now.Sub(*r.StartedAt).Milliseconds()
	r.FinishedAt = &now
	r.UpdatedAt = now
	r.Version++
	return nil
}

// Fail 標記執行失敗。
func (r *Run) Fail(reason string, now time.Time) error {
	if r.Status == RunCompleted {
		return ErrInvalidTransition.WithMessage("run %s is already completed", r.PublicID)
	}
	r.Status = RunFailed
	r.Error = reason
	r.FinishedAt = &now
	r.UpdatedAt = now
	r.Version++
	return nil
}

// SettlementDate 回傳 period_start 的日期（YYYY-MM-DD，UTC）。
func (r *Run) SettlementDate() string { return r.PeriodStart.UTC().Format("2006-01-02") }

// PeriodForDate 把結算日（YYYY-MM-DD）展開成 [當日 00:00Z, 次日 00:00Z)。
func PeriodForDate(date string) (start, end time.Time, err error) {
	d, err := time.Parse("2006-01-02", strings.TrimSpace(date))
	if err != nil {
		return time.Time{}, time.Time{}, ErrInvalidPeriod.WithMessage("settlement_date must be YYYY-MM-DD, got %q.", date)
	}
	start = d.UTC()
	return start, start.Add(24 * time.Hour), nil
}

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
)

// RunRepo 實作 app.RunRepo（reconciliation_runs）。
type RunRepo struct{ pool *pgxpool.Pool }

const runColumns = `id, public_id, provider, period_start, period_end, status, matched_count, unmatched_count, summary,
	error, started_at, finished_at, triggered_by, created_at, updated_at, version`

// Create 實作 app.RunRepo。
func (r *RunRepo) Create(ctx context.Context, run *domain.Run) error {
	summary, err := json.Marshal(run.Summary)
	if err != nil {
		return fmt.Errorf("postgres: marshal run summary: %w", err)
	}
	_, err = q(ctx, r.pool).Exec(ctx, `
		INSERT INTO reconciliation_runs (id, public_id, provider, period_start, period_end, status, matched_count, unmatched_count,
			summary, error, started_at, finished_at, triggered_by, created_at, updated_at, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, NULLIF($10, ''), $11, $12, $13, $14, $15, $16)`,
		run.ID, run.PublicID, run.Provider, run.PeriodStart, run.PeriodEnd, string(run.Status), run.MatchedCount, run.UnmatchedCount,
		summary, run.Error, run.StartedAt, run.FinishedAt, run.TriggeredBy, run.CreatedAt, run.UpdatedAt, run.Version)
	if err != nil {
		return fmt.Errorf("postgres: insert reconciliation_run: %w", err)
	}
	return nil
}

// Update 實作 app.RunRepo。
//
// domain 的 Start / Complete / Fail 會自行 Version++，因此期望版本為 run.Version-1（至少一次轉移）。
func (r *RunRepo) Update(ctx context.Context, run *domain.Run) error {
	summary, err := json.Marshal(run.Summary)
	if err != nil {
		return fmt.Errorf("postgres: marshal run summary: %w", err)
	}
	tag, err := q(ctx, r.pool).Exec(ctx, `
		UPDATE reconciliation_runs
		   SET status = $2, matched_count = $3, unmatched_count = $4, summary = $5::jsonb, error = NULLIF($6, ''),
		       started_at = $7, finished_at = $8, version = $9
		 WHERE id = $1 AND version < $9`,
		run.ID, string(run.Status), run.MatchedCount, run.UnmatchedCount, summary, run.Error,
		run.StartedAt, run.FinishedAt, run.Version)
	return optimistic(tag, err, "reconciliation_run")
}

// GetByID 實作 app.RunRepo。
func (r *RunRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Run, error) {
	rows, err := q(ctx, r.pool).Query(ctx, `SELECT `+runColumns+` FROM reconciliation_runs WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: get reconciliation_run: %w", err)
	}
	runs, err := scanRuns(rows)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, domain.ErrRunNotFound
	}
	return &runs[0], nil
}

// FindByFileID 實作 app.RunRepo（summary->>'file_id'；每檔 run 數極少，不需索引）。
func (r *RunRepo) FindByFileID(ctx context.Context, fileID uuid.UUID) (*domain.Run, error) {
	rows, err := q(ctx, r.pool).Query(ctx, `
		SELECT `+runColumns+` FROM reconciliation_runs
		 WHERE summary->>'file_id' = $1 ORDER BY created_at DESC LIMIT 1`, fileID.String())
	if err != nil {
		return nil, fmt.Errorf("postgres: find run by file: %w", err)
	}
	runs, err := scanRuns(rows)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// List 實作 app.RunRepo（keyset 分頁：created_at DESC, id DESC）。
func (r *RunRepo) List(ctx context.Context, f app.RunFilter) ([]domain.Run, string, error) {
	cur, err := decodeCursor(f.PageToken)
	if err != nil {
		return nil, "", err
	}
	var w whereBuilder
	if f.Provider != "" {
		w.add("provider = $%d", f.Provider)
	}
	if len(f.Statuses) > 0 {
		st := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			st[i] = string(s)
		}
		w.add("status = ANY($%d)", st)
	}
	if !f.DateFrom.IsZero() {
		w.add("period_start >= $%d", f.DateFrom.UTC())
	}
	if !f.DateTo.IsZero() {
		w.add("period_start < $%d", f.DateTo.UTC().Add(24*time.Hour))
	}
	if cur != nil {
		w.add("(created_at, id) < ($%d, $%d)", cur.CreatedAt, cur.ID)
	}
	size := f.PageSize
	if size <= 0 {
		size = app.DefaultPageSize
	}
	limit := w.next(size + 1)
	rows, err := q(ctx, r.pool).Query(ctx,
		`SELECT `+runColumns+` FROM reconciliation_runs`+w.sql()+fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, limit),
		w.args...)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list reconciliation_runs: %w", err)
	}
	runs, err := scanRuns(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(runs) > size {
		runs = runs[:size]
		last := runs[len(runs)-1]
		next = encodeCursor(cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return runs, next, nil
}

func scanRuns(rows pgx.Rows) ([]domain.Run, error) {
	defer rows.Close()
	var out []domain.Run
	for rows.Next() {
		var (
			run     domain.Run
			status  string
			summary []byte
			errText *string
		)
		if err := rows.Scan(&run.ID, &run.PublicID, &run.Provider, &run.PeriodStart, &run.PeriodEnd, &status, &run.MatchedCount,
			&run.UnmatchedCount, &summary, &errText, &run.StartedAt, &run.FinishedAt, &run.TriggeredBy, &run.CreatedAt, &run.UpdatedAt, &run.Version); err != nil {
			return nil, fmt.Errorf("postgres: scan reconciliation_run: %w", err)
		}
		run.Status = domain.RunStatus(status)
		run.Error = deref(errText)
		if len(summary) > 0 {
			_ = json.Unmarshal(summary, &run.Summary)
		}
		if run.Summary.ByKind == nil {
			run.Summary.ByKind = map[string]int{}
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}

var _ app.RunRepo = (*RunRepo)(nil)

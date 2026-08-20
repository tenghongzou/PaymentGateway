package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
)

// DiscrepancyRepo 實作 app.DiscrepancyRepo（discrepancies）。
//
// fee_mismatch 的落地：discrepancies.kind CHECK 目前沒有 'fee_mismatch'，故以
// kind='amount_mismatch' + details.kind='fee_mismatch' 儲存，讀取時還原。
// TODO(migration)：CHECK 加入 fee_mismatch 後移除 dbKind / kindCond 的對應。
type DiscrepancyRepo struct{ pool *pgxpool.Pool }

const discrepancyColumns = `id, run_id, merchant_id, kind, provider, provider_reference, internal_reference, settlement_line_id,
	expected_amount, actual_amount, currency, status, resolution_note, resolved_by, resolved_at, details, created_at, updated_at, version`

// dbKind 回傳寫入 kind 欄位的值與是否需要在 details 標記真正的 kind。
func dbKind(k domain.DiscrepancyKind) (string, bool) {
	if k == domain.KindFeeMismatch {
		return string(domain.KindAmountMismatch), true
	}
	return string(k), false
}

func marshalDetails(d *domain.Discrepancy) ([]byte, error) {
	details := map[string]any{}
	for k, v := range d.Details {
		details[k] = v
	}
	if _, override := dbKind(d.Kind); override {
		details[domain.DetailKind] = string(d.Kind)
	} else {
		delete(details, domain.DetailKind)
	}
	return json.Marshal(details)
}

// InsertBatch 實作 app.DiscrepancyRepo。
func (r *DiscrepancyRepo) InsertBatch(ctx context.Context, ds []domain.Discrepancy) error {
	if len(ds) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for i := range ds {
		d := &ds[i]
		details, err := marshalDetails(d)
		if err != nil {
			return fmt.Errorf("postgres: marshal discrepancy details: %w", err)
		}
		kind, _ := dbKind(d.Kind)
		batch.Queue(`
			INSERT INTO discrepancies (id, run_id, merchant_id, kind, provider, provider_reference, internal_reference, settlement_line_id,
				expected_amount, actual_amount, currency, status, resolution_note, resolved_by, resolved_at, details, created_at, updated_at, version)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10, NULLIF($11, ''), $12, NULLIF($13, ''), NULLIF($14, ''), $15, $16::jsonb, $17, $18, $19)`,
			d.ID, d.RunID, d.MerchantID, kind, d.Provider, d.ProviderReference, d.InternalReference, d.SettlementLineID,
			d.ExpectedAmount, d.ActualAmount, d.Currency, string(d.Status), d.ResolutionNote, d.ResolvedBy, d.ResolvedAt, details,
			d.CreatedAt, d.UpdatedAt, d.Version)
	}
	res := q(ctx, r.pool).(interface {
		SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	}).SendBatch(ctx, batch)
	defer res.Close()
	for range ds {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("postgres: insert discrepancy: %w", err)
		}
	}
	return nil
}

// GetByID 實作 app.DiscrepancyRepo。
func (r *DiscrepancyRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Discrepancy, error) {
	rows, err := q(ctx, r.pool).Query(ctx, `SELECT `+discrepancyColumns+` FROM discrepancies WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: get discrepancy: %w", err)
	}
	ds, err := scanDiscrepancies(rows)
	if err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		return nil, domain.ErrDiscrepancyNotFound
	}
	return &ds[0], nil
}

// Update 實作 app.DiscrepancyRepo（domain.Resolve / Ignore 已 Version++，期望 DB 版本 < 新版本）。
func (r *DiscrepancyRepo) Update(ctx context.Context, d *domain.Discrepancy) error {
	details, err := marshalDetails(d)
	if err != nil {
		return fmt.Errorf("postgres: marshal discrepancy details: %w", err)
	}
	tag, err := q(ctx, r.pool).Exec(ctx, `
		UPDATE discrepancies
		   SET status = $2, resolution_note = NULLIF($3, ''), resolved_by = NULLIF($4, ''), resolved_at = $5,
		       details = $6::jsonb, version = $7
		 WHERE id = $1 AND version < $7`,
		d.ID, string(d.Status), d.ResolutionNote, d.ResolvedBy, d.ResolvedAt, details, d.Version)
	return optimistic(tag, err, "discrepancy")
}

// kindCond 組出某 kind 的 WHERE 條件（處理 fee_mismatch 對應）。
func kindCond(w *whereBuilder, kinds []domain.DiscrepancyKind) {
	if len(kinds) == 0 {
		return
	}
	var plain []string
	var extra []string
	for _, k := range kinds {
		switch k {
		case domain.KindFeeMismatch:
			extra = append(extra, "(kind = 'amount_mismatch' AND details->>'kind' = 'fee_mismatch')")
		case domain.KindAmountMismatch:
			extra = append(extra, "(kind = 'amount_mismatch' AND (details->>'kind') IS DISTINCT FROM 'fee_mismatch')")
		default:
			plain = append(plain, string(k))
		}
	}
	cond := ""
	if len(plain) > 0 {
		w.args = append(w.args, plain)
		cond = fmt.Sprintf("kind = ANY($%d)", len(w.args))
	}
	for _, e := range extra {
		if cond != "" {
			cond += " OR "
		}
		cond += e
	}
	w.conds = append(w.conds, "("+cond+")")
}

// List 實作 app.DiscrepancyRepo（keyset 分頁）。
func (r *DiscrepancyRepo) List(ctx context.Context, f app.DiscrepancyFilter) ([]domain.Discrepancy, string, error) {
	cur, err := decodeCursor(f.PageToken)
	if err != nil {
		return nil, "", err
	}
	var w whereBuilder
	if f.RunID != nil {
		w.add("run_id = $%d", *f.RunID)
	}
	if f.MerchantID != nil {
		w.add("merchant_id = $%d", *f.MerchantID)
	}
	if f.Provider != "" {
		w.add("provider = $%d", f.Provider)
	}
	kindCond(&w, f.Kinds)
	if len(f.Statuses) > 0 {
		st := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			st[i] = string(s)
		}
		w.add("status = ANY($%d)", st)
	}
	if f.PaymentID != "" {
		w.add("internal_reference = $%d", f.PaymentID)
	}
	if !f.CreatedAfter.IsZero() {
		w.add("created_at > $%d", f.CreatedAfter)
	}
	if !f.CreatedBefore.IsZero() {
		w.add("created_at < $%d", f.CreatedBefore)
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
		`SELECT `+discrepancyColumns+` FROM discrepancies`+w.sql()+fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, limit),
		w.args...)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list discrepancies: %w", err)
	}
	ds, err := scanDiscrepancies(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(ds) > size {
		ds = ds[:size]
		last := ds[len(ds)-1]
		next = encodeCursor(cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return ds, next, nil
}

// ExistsOpen 實作 app.DiscrepancyRepo。
func (r *DiscrepancyRepo) ExistsOpen(ctx context.Context, provider string, kind domain.DiscrepancyKind, providerRef, internalRef string) (bool, error) {
	if providerRef == "" && internalRef == "" {
		return false, nil
	}
	var w whereBuilder
	w.add("provider = $%d", provider)
	w.add("status = 'open'")
	kindCond(&w, []domain.DiscrepancyKind{kind})
	w.add("((provider_reference = $%d AND $%d <> '') OR (internal_reference = $%d AND $%d <> ''))", providerRef, providerRef, internalRef, internalRef)
	var exists bool
	err := q(ctx, r.pool).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM discrepancies`+w.sql()+`)`, w.args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("postgres: exists open discrepancy: %w", err)
	}
	return exists, nil
}

func scanDiscrepancies(rows pgx.Rows) ([]domain.Discrepancy, error) {
	defer rows.Close()
	var out []domain.Discrepancy
	for rows.Next() {
		var (
			d               domain.Discrepancy
			kind, status    string
			pref, iref, cur *string
			note, by        *string
			details         []byte
		)
		if err := rows.Scan(&d.ID, &d.RunID, &d.MerchantID, &kind, &d.Provider, &pref, &iref, &d.SettlementLineID,
			&d.ExpectedAmount, &d.ActualAmount, &cur, &status, &note, &by, &d.ResolvedAt, &details, &d.CreatedAt, &d.UpdatedAt, &d.Version); err != nil {
			return nil, fmt.Errorf("postgres: scan discrepancy: %w", err)
		}
		d.Kind = domain.DiscrepancyKind(kind)
		d.Status = domain.DiscrepancyStatus(status)
		d.ProviderReference, d.InternalReference, d.Currency = deref(pref), deref(iref), deref(cur)
		d.ResolutionNote, d.ResolvedBy = deref(note), deref(by)
		d.Details = map[string]any{}
		if len(details) > 0 {
			_ = json.Unmarshal(details, &d.Details)
		}
		if real := d.Detail(domain.DetailKind); real != "" && domain.DiscrepancyKind(real).IsValid() {
			d.Kind = domain.DiscrepancyKind(real)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}

var _ app.DiscrepancyRepo = (*DiscrepancyRepo)(nil)

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// PaymentRecordRepo 實作 app.PaymentRecordRepo（payment_records 讀模型）。
//
// TODO(migration)：payment_records 沒有 fee 欄位，PaymentRecord.Fee 不會被持久化（讀回為 nil）。
type PaymentRecordRepo struct{ pool *pgxpool.Pool }

const recordColumns = `id, kind, public_id, merchant_id, provider, provider_reference, amount, currency, status, occurred_at, source_seq, created_at, updated_at`

// Upsert 實作 app.PaymentRecordRepo：source_seq 較舊的事件不套用（docs/04 §9 讀模型規則）。
func (r *PaymentRecordRepo) Upsert(ctx context.Context, rec *domain.PaymentRecord) (bool, error) {
	tag, err := q(ctx, r.pool).Exec(ctx, `
		INSERT INTO payment_records (id, kind, public_id, merchant_id, provider, provider_reference, amount, currency, status, occurred_at, source_seq, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (id) DO UPDATE
		   SET kind = EXCLUDED.kind, public_id = EXCLUDED.public_id, merchant_id = EXCLUDED.merchant_id,
		       provider = COALESCE(EXCLUDED.provider, payment_records.provider),
		       provider_reference = COALESCE(EXCLUDED.provider_reference, payment_records.provider_reference),
		       amount = EXCLUDED.amount, currency = EXCLUDED.currency, status = EXCLUDED.status,
		       occurred_at = EXCLUDED.occurred_at, source_seq = EXCLUDED.source_seq
		 WHERE payment_records.source_seq <= EXCLUDED.source_seq`,
		rec.ID, string(rec.Kind), rec.PublicID, rec.MerchantID, rec.Provider, rec.ProviderReference,
		rec.Amount.AmountMinor, rec.Amount.Currency, rec.Status, rec.OccurredAt, rec.SourceSeq, rec.CreatedAt, rec.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("postgres: upsert payment_record: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Get 實作 app.PaymentRecordRepo。
func (r *PaymentRecordRepo) Get(ctx context.Context, id uuid.UUID) (*domain.PaymentRecord, error) {
	rows, err := q(ctx, r.pool).Query(ctx, `SELECT `+recordColumns+` FROM payment_records WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: get payment_record: %w", err)
	}
	recs, err := scanRecords(rows)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return &recs[0], nil
}

// FindByProviderRefs 實作 app.PaymentRecordRepo。
func (r *PaymentRecordRepo) FindByProviderRefs(ctx context.Context, provider string, refs []string) ([]domain.PaymentRecord, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	rows, err := q(ctx, r.pool).Query(ctx, `
		SELECT `+recordColumns+` FROM payment_records
		 WHERE provider = $1 AND provider_reference = ANY($2)`, provider, refs)
	if err != nil {
		return nil, fmt.Errorf("postgres: find payment_records: %w", err)
	}
	return scanRecords(rows)
}

// ListUnsettled 實作 app.PaymentRecordRepo：本地 JOIN settlement_lines，找出應結算但沒有任何結算列對上的紀錄。
func (r *PaymentRecordRepo) ListUnsettled(ctx context.Context, provider string, before time.Time, limit int) ([]domain.PaymentRecord, error) {
	if limit <= 0 {
		limit = 10000
	}
	st := domain.SettleableStatuses()
	rows, err := q(ctx, r.pool).Query(ctx, `
		SELECT `+qualify(recordColumns, "pr")+`
		  FROM payment_records pr
		 WHERE pr.provider = $1
		   AND pr.provider_reference IS NOT NULL
		   AND pr.occurred_at < $2
		   AND ((pr.kind = 'payment' AND pr.status = ANY($3))
		     OR (pr.kind = 'refund'  AND pr.status = ANY($4))
		     OR (pr.kind = 'dispute' AND pr.status = ANY($5)))
		   AND NOT EXISTS (
		         SELECT 1 FROM settlement_lines sl
		          WHERE sl.provider = pr.provider
		            AND sl.provider_reference = pr.provider_reference
		            AND sl.type = CASE pr.kind WHEN 'payment' THEN 'payment' WHEN 'refund' THEN 'refund' ELSE 'chargeback' END)
		 ORDER BY pr.occurred_at
		 LIMIT $6`,
		provider, before, st[domain.RecordPayment], st[domain.RecordRefund], st[domain.RecordDispute], limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list unsettled payment_records: %w", err)
	}
	return scanRecords(rows)
}

func scanRecords(rows pgx.Rows) ([]domain.PaymentRecord, error) {
	defer rows.Close()
	var out []domain.PaymentRecord
	for rows.Next() {
		var (
			rec           domain.PaymentRecord
			kind          string
			provider, ref *string
			amount        int64
			currency      string
		)
		if err := rows.Scan(&rec.ID, &kind, &rec.PublicID, &rec.MerchantID, &provider, &ref, &amount, &currency,
			&rec.Status, &rec.OccurredAt, &rec.SourceSeq, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan payment_record: %w", err)
		}
		rec.Kind = domain.RecordKind(kind)
		rec.Provider = deref(provider)
		rec.ProviderReference = deref(ref)
		rec.Amount = money.Money{AmountMinor: amount, Currency: currency}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}

// qualify 把 "a, b, c" 變成 "t.a, t.b, t.c"。
func qualify(cols, alias string) string {
	out := ""
	for i, c := range splitCols(cols) {
		if i > 0 {
			out += ", "
		}
		out += alias + "." + c
	}
	return out
}

func splitCols(cols string) []string {
	var out []string
	cur := ""
	for _, ch := range cols {
		switch ch {
		case ',':
			out = append(out, trimSpace(cur))
			cur = ""
		default:
			cur += string(ch)
		}
	}
	if t := trimSpace(cur); t != "" {
		out = append(out, t)
	}
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

var _ app.PaymentRecordRepo = (*PaymentRecordRepo)(nil)

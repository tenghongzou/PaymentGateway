package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// LineRepo 實作 app.LineRepo（settlement_lines）。
type LineRepo struct{ pool *pgxpool.Pool }

// rawEnvelope 為 settlement_lines.raw 的結構：保留原始欄位，另存手續費（表上沒有 fee 欄位）。
type rawEnvelope struct {
	FeeMinor int64             `json:"fee_minor"`
	Fields   map[string]string `json:"fields"`
}

// InsertBatch 實作 app.LineRepo（批次 INSERT，(file_id, line_no) 衝突略過）。
func (r *LineRepo) InsertBatch(ctx context.Context, lines []domain.SettlementLine) error {
	if len(lines) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, l := range lines {
		raw, err := json.Marshal(rawEnvelope{FeeMinor: l.Fee.AmountMinor, Fields: nonNilMap(l.Raw)})
		if err != nil {
			return fmt.Errorf("postgres: marshal line raw: %w", err)
		}
		batch.Queue(`
			INSERT INTO settlement_lines (id, file_id, line_no, provider, provider_reference, merchant_reference,
				type, amount, currency, settled_at, raw, created_at)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11::jsonb, $12)
			ON CONFLICT (file_id, line_no) DO NOTHING`,
			l.ID, l.FileID, l.LineNo, l.Provider, l.ProviderReference, l.MerchantReference,
			string(l.Type), l.Amount.AmountMinor, l.Amount.Currency, l.SettledAt, raw, l.CreatedAt)
	}
	res := q(ctx, r.pool).(interface {
		SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	}).SendBatch(ctx, batch)
	defer res.Close()
	for range lines {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("postgres: insert settlement_line: %w", err)
		}
	}
	return nil
}

// ListByFile 實作 app.LineRepo。
func (r *LineRepo) ListByFile(ctx context.Context, fileID uuid.UUID) ([]domain.SettlementLine, error) {
	rows, err := q(ctx, r.pool).Query(ctx, `
		SELECT id, file_id, line_no, provider, provider_reference, merchant_reference, type, amount, currency, settled_at, raw, created_at
		  FROM settlement_lines WHERE file_id = $1 ORDER BY line_no`, fileID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list settlement_lines: %w", err)
	}
	defer rows.Close()
	var out []domain.SettlementLine
	for rows.Next() {
		var (
			l        domain.SettlementLine
			mref     *string
			typ      string
			amount   int64
			currency string
			raw      []byte
		)
		if err := rows.Scan(&l.ID, &l.FileID, &l.LineNo, &l.Provider, &l.ProviderReference, &mref, &typ, &amount, &currency, &l.SettledAt, &raw, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan settlement_line: %w", err)
		}
		l.MerchantReference = deref(mref)
		l.Type = domain.LineType(typ)
		l.Amount = money.Money{AmountMinor: amount, Currency: currency}
		var env rawEnvelope
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &env)
		}
		l.Fee = money.Money{AmountMinor: env.FeeMinor, Currency: currency}
		l.Raw = env.Fields
		out = append(out, l)
	}
	return out, rows.Err()
}

var _ app.LineRepo = (*LineRepo)(nil)

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/ledger/app"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
)

// AccountRepo 實作 app.AccountRepo。
type AccountRepo struct {
	pool *pgxpool.Pool
}

// NewAccountRepo 建立 AccountRepo。
func NewAccountRepo(pool *pgxpool.Pool) *AccountRepo { return &AccountRepo{pool: pool} }

const accountCols = `id, merchant_id, code, name, type, normal_balance, currency, status, metadata, version, created_at, updated_at`

// scanAccount 把一列 accounts 轉成 domain.Account。
func scanAccount(row pgx.Row) (*domain.Account, error) {
	var (
		a          domain.Account
		merchantID *uuid.UUID
		code       string
		meta       []byte
	)
	if err := row.Scan(&a.ID, &merchantID, &code, &a.Name, &a.Type, &a.NormalBalance, &a.Key.Currency, &a.Status, &meta, &a.Version, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	if merchantID != nil {
		a.Key.MerchantID = *merchantID
	}
	a.Key.Code, a.Key.Livemode = domain.ParseStorageCode(code)
	a.Key.Currency = strings.TrimSpace(a.Key.Currency)
	a.Metadata = map[string]string{}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &a.Metadata)
	}
	return &a, nil
}

func nullableMerchant(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// EnsureAccount 以 INSERT ... ON CONFLICT DO NOTHING 建立帳戶（lazy create），再讀回。
func (r *AccountRepo) EnsureAccount(ctx context.Context, key domain.AccountKey) (*domain.Account, bool, error) {
	acct, err := domain.NewAccount(key)
	if err != nil {
		return nil, false, err
	}
	acct.Metadata["livemode"] = fmt.Sprintf("%t", key.Livemode)
	meta, _ := json.Marshal(acct.Metadata)
	q := querierFrom(ctx, r.pool)
	row := q.QueryRow(ctx, `
		INSERT INTO accounts (merchant_id, code, name, type, normal_balance, currency, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		ON CONFLICT (merchant_id, code, currency) DO NOTHING
		RETURNING `+accountCols,
		nullableMerchant(key.MerchantID), domain.StorageCode(key.Code, key.Livemode), acct.Name,
		string(acct.Type), string(acct.NormalBalance), key.Currency, meta)
	created, err := scanAccount(row)
	switch {
	case err == nil:
		return created, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, false, translateError(fmt.Errorf("ledger/postgres: ensure account: %w", err))
	}
	existing, err := r.GetByKey(ctx, key)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

// GetByID 依 uuid 取得帳戶。
func (r *AccountRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Account, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = $1`, id)
	a, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ledger/postgres: get account: %w", err)
	}
	return a, nil
}

// GetByKey 依自然鍵取得帳戶。
func (r *AccountRepo) GetByKey(ctx context.Context, key domain.AccountKey) (*domain.Account, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT `+accountCols+` FROM accounts
		 WHERE merchant_id IS NOT DISTINCT FROM $1 AND code = $2 AND currency = $3`,
		nullableMerchant(key.MerchantID), domain.StorageCode(key.Code, key.Livemode), key.Currency)
	a, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ledger/postgres: get account by key: %w", err)
	}
	return a, nil
}

// List 依條件列出帳戶（keyset 分頁：created_at, id 升冪）。
func (r *AccountRepo) List(ctx context.Context, f app.AccountFilter, page app.Page) ([]*domain.Account, *app.Cursor, error) {
	var (
		where []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.MerchantID != nil {
		where = append(where, "merchant_id = "+arg(*f.MerchantID))
	}
	if f.SystemOnly {
		where = append(where, "merchant_id IS NULL")
	}
	// code 可能帶 test: 前綴；以 regexp_replace 去前綴後比對種類 / 後綴。
	if f.Kind != "" {
		where = append(where, "split_part(regexp_replace(code, '^test:', ''), ':', 1) = "+arg(string(f.Kind)))
	}
	if f.Qualifier != "" {
		where = append(where, "split_part(regexp_replace(code, '^test:', ''), ':', 2) = "+arg(f.Qualifier))
	}
	if f.Currency != "" {
		where = append(where, "currency = "+arg(f.Currency))
	}
	if f.Livemode != nil {
		where = append(where, "(code NOT LIKE 'test:%') = "+arg(*f.Livemode))
	}
	if page.After != nil {
		where = append(where, fmt.Sprintf("(created_at, id) > (%s, %s)", arg(page.After.At), arg(page.After.ID)))
	}
	limit := page.Limit
	if limit <= 0 {
		limit = app.DefaultPageSize
	}
	sql := `SELECT ` + accountCols + ` FROM accounts`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY created_at, id LIMIT " + arg(limit+1)

	rows, err := querierFrom(ctx, r.pool).Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger/postgres: list accounts: %w", err)
	}
	defer rows.Close()
	var out []*domain.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("ledger/postgres: scan account: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("ledger/postgres: list accounts rows: %w", err)
	}
	var next *app.Cursor
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = &app.Cursor{At: last.CreatedAt, ID: last.ID}
	}
	return out, next, nil
}

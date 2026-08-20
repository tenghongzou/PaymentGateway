package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
)

// BalanceRepo 實作 app.BalanceRepo（讀 balances 表；借貸合計由 entries 彙總）。
type BalanceRepo struct {
	pool *pgxpool.Pool
}

// NewBalanceRepo 建立 BalanceRepo。
func NewBalanceRepo(pool *pgxpool.Pool) *BalanceRepo { return &BalanceRepo{pool: pool} }

const balanceSelect = `
	SELECT a.id, a.merchant_id, a.code, a.currency, a.type, a.normal_balance,
	       b.balance, b.entry_count, b.as_of_entry_id, b.version, b.updated_at,
	       (SELECT e.journal_id FROM entries e WHERE e.id = b.as_of_entry_id LIMIT 1)                                   AS as_of_journal_id,
	       COALESCE((SELECT SUM(e.amount) FROM entries e WHERE e.account_id = a.id AND e.direction = 'debit'), 0)::bigint  AS total_debit,
	       COALESCE((SELECT SUM(e.amount) FROM entries e WHERE e.account_id = a.id AND e.direction = 'credit'), 0)::bigint AS total_credit
	  FROM accounts a
	  JOIN balances b ON b.account_id = a.id`

func scanBalance(row pgx.Row) (*domain.Balance, error) {
	var (
		b          domain.Balance
		merchantID *uuid.UUID
		code       string
	)
	if err := row.Scan(&b.AccountID, &merchantID, &code, &b.Account.Currency, &b.Type, &b.NormalBalance,
		&b.Balance, &b.EntryCount, &b.AsOfEntryID, &b.Version, &b.UpdatedAt,
		&b.AsOfJournalID, &b.TotalDebit, &b.TotalCredit); err != nil {
		return nil, err
	}
	if merchantID != nil {
		b.Account.MerchantID = *merchantID
	}
	b.Account.Code, b.Account.Livemode = domain.ParseStorageCode(code)
	b.Account.Currency = strings.TrimSpace(b.Account.Currency)
	return &b, nil
}

// GetByAccount 取得單一帳戶餘額。
func (r *BalanceRepo) GetByAccount(ctx context.Context, accountID uuid.UUID) (*domain.Balance, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx, balanceSelect+` WHERE a.id = $1`, accountID)
	b, err := scanBalance(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ledger/postgres: get balance: %w", err)
	}
	return b, nil
}

// ListByMerchant 取得商戶所有帳戶餘額（currency 空 = 全部）。
func (r *BalanceRepo) ListByMerchant(ctx context.Context, merchantID uuid.UUID, currency string, livemode bool) ([]*domain.Balance, error) {
	rows, err := querierFrom(ctx, r.pool).Query(ctx, balanceSelect+`
		 WHERE a.merchant_id = $1
		   AND (a.code NOT LIKE 'test:%') = $2
		   AND ($3 = '' OR a.currency = $3)
		 ORDER BY a.currency, a.code`, merchantID, livemode, currency)
	if err != nil {
		return nil, fmt.Errorf("ledger/postgres: list merchant balances: %w", err)
	}
	defer rows.Close()
	var out []*domain.Balance
	for rows.Next() {
		b, err := scanBalance(rows)
		if err != nil {
			return nil, fmt.Errorf("ledger/postgres: scan balance: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger/postgres: merchant balances rows: %w", err)
	}
	return out, nil
}

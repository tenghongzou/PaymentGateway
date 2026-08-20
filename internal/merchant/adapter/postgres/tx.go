// Package postgres 為 merchant-service 的 PostgreSQL adapter：實作 app 的 repo / outbox ports（SQL 對齊 migrations/merchant）。
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// txKey 為 ctx 內攜帶 pgx.Tx 的 key。
type txKey struct{}

// DB 為 pool 與 tx 共同滿足的最小查詢介面。
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TxManager 實作 app.TxManager：交易透過 ctx 傳遞給同一 use case 內的所有 repo 呼叫。
type TxManager struct {
	pool *pgxpool.Pool
}

// NewTxManager 建立 TxManager。
func NewTxManager(pool *pgxpool.Pool) *TxManager { return &TxManager{pool: pool} }

// WithinTx 開啟交易（ctx 已帶交易時沿用）。
func (m *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := txFrom(ctx); ok {
		return fn(ctx)
	}
	return pgdb.WithTx(ctx, m.pool, func(tx pgx.Tx) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

// txFrom 取出 ctx 內的交易。
func txFrom(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// dbFrom 回傳 ctx 內的交易，否則 pool。
func dbFrom(ctx context.Context, pool *pgxpool.Pool) DB {
	if tx, ok := txFrom(ctx); ok {
		return tx
	}
	return pool
}

// Repos 為所有 adapter 的集合（main 組裝用）。
type Repos struct {
	Tx        *TxManager
	Merchants *MerchantRepo
	APIKeys   *ApiKeyRepo
	Webhooks  *WebhookEndpointRepo
	Routing   *RoutingPrefRepo
	Outbox    *OutboxStore
}

// NewRepos 以同一個 pool 建立所有 adapter。
func NewRepos(pool *pgxpool.Pool) *Repos {
	return &Repos{
		Tx:        NewTxManager(pool),
		Merchants: NewMerchantRepo(pool),
		APIKeys:   NewApiKeyRepo(pool),
		Webhooks:  NewWebhookEndpointRepo(pool),
		Routing:   NewRoutingPrefRepo(pool),
		Outbox:    NewOutboxStore(),
	}
}

// mapErr 把 pgx / pgdb 錯誤轉成領域錯誤。
func mapErr(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, pgdb.ErrNotFound):
		return domain.ErrNotFound
	case pgdb.IsUniqueViolation(err):
		return domain.ErrAlreadyExists.WithMessage("unique constraint %s violated", pgdb.ConstraintName(err)).Wrap(err)
	case pgdb.IsCheckViolation(err):
		return domain.ErrParameterInvalid.WithMessage("constraint %s violated", pgdb.ConstraintName(err)).Wrap(err)
	default:
		return fmt.Errorf("postgres: %s: %w", op, err)
	}
}

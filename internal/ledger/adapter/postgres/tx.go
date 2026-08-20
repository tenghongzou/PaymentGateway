// Package postgres 為 ledger-service 的 repository 實作（schema 事實來源：migrations/ledger）。
//
// 交易以 context 傳遞：TxRunner.RunInTx 把 pgx.Tx 放進 ctx，各 repo 以 querier(ctx) 取得交易或連線池。
// 帳本 append-only：本套件對 journals / entries 只有 INSERT / SELECT（CI 以 grep 把關）。
package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

type txKey struct{}

// TxRunner 實作 app.TxRunner。
type TxRunner struct {
	pool *pgxpool.Pool
}

// NewTxRunner 建立 TxRunner。
func NewTxRunner(pool *pgxpool.Pool) *TxRunner { return &TxRunner{pool: pool} }

// RunInTx 在交易內執行 fn；已在交易內時直接加入既有交易（不開 savepoint）。
// commit 時 deferred trigger 拋出的錯誤也會被轉成領域錯誤。
func (r *TxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := TxFromContext(ctx); ok {
		return fn(ctx)
	}
	err := pgdb.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
	return translateError(err)
}

// TxFromContext 取出 ctx 內的交易。
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// querier 為 pgx.Tx 與 *pgxpool.Pool 的共同子集。
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// querierFrom 回傳 ctx 內的交易，否則回傳連線池。
func querierFrom(ctx context.Context, pool *pgxpool.Pool) querier {
	if tx, ok := TxFromContext(ctx); ok {
		return tx
	}
	return pool
}

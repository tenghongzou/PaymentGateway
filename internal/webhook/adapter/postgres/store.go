// Package postgres 為 webhook-service 的 repository 實作（schema 以 migrations/webhook 為準）。
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

type txKey struct{}

// querier 為 pool 與 tx 的共同介面。
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store 持有連線池並實作 app.Transactor；各 repo 透過 ctx 取得交易。
type Store struct {
	pool *pgxpool.Pool
}

// NewStore 建立 Store。
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Pool 回傳底層連線池。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping 供 readiness 檢查。
func (s *Store) Ping(ctx context.Context) error { return pgdb.Ping(ctx, s.pool) }

// Close 關閉連線池。
func (s *Store) Close(context.Context) error {
	s.pool.Close()
	return nil
}

// InTx 在交易內執行 fn；ctx 已帶交易時直接沿用（可重入）。
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}
	return pgdb.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

// q 回傳 ctx 內的交易，否則回連線池。
func (s *Store) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return s.pool
}

// txFrom 取出 ctx 內的交易（沒有時回錯誤）。
func txFrom(ctx context.Context) (pgx.Tx, error) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		return nil, errors.New("postgres: operation requires a transaction (use Store.InTx)")
	}
	return tx, nil
}

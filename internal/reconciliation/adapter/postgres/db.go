// Package postgres 為 reconciliation-service 的 repository 實作（schema 以 migrations/reconciliation 為準）。
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// querier 為 pgx.Tx 與 *pgxpool.Pool 的共同子集。
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{}

// TxManager 實作 app.TxManager：把 pgx.Tx 放進 ctx，repo 透過 q(ctx) 取用。
type TxManager struct{ pool *pgxpool.Pool }

// NewTxManager 建立 TxManager。
func NewTxManager(pool *pgxpool.Pool) *TxManager { return &TxManager{pool: pool} }

// WithinTx 實作 app.TxManager；巢狀呼叫重用外層交易。
func (m *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
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

// Repos 把所有 repository 綁在同一個 pool 上。
type Repos struct {
	pool *pgxpool.Pool
	Tx   *TxManager
	*FileRepo
	*LineRepo
	*PaymentRecordRepo
	*RunRepo
	*DiscrepancyRepo
	*Inbox
	*Outbox
}

// NewRepos 建立整組 repository。
func NewRepos(pool *pgxpool.Pool) *Repos {
	return &Repos{
		pool:              pool,
		Tx:                NewTxManager(pool),
		FileRepo:          &FileRepo{pool: pool},
		LineRepo:          &LineRepo{pool: pool},
		PaymentRecordRepo: &PaymentRecordRepo{pool: pool},
		RunRepo:           &RunRepo{pool: pool},
		DiscrepancyRepo:   &DiscrepancyRepo{pool: pool},
		Inbox:             &Inbox{inbox: outbox.NewInbox()},
		Outbox:            &Outbox{store: outbox.NewStore()},
	}
}

// Deps 組出 app.Deps（Clock / Logger 由呼叫端補）。
func (r *Repos) Deps() app.Deps {
	return app.Deps{
		Tx: r.Tx, Files: r.FileRepo, Lines: r.LineRepo, Records: r.PaymentRecordRepo,
		Runs: r.RunRepo, Discrepancies: r.DiscrepancyRepo, Inbox: r.Inbox, Outbox: r.Outbox,
	}
}

// q 回傳 ctx 內的交易，否則回傳 pool。
func q(ctx context.Context, pool *pgxpool.Pool) querier {
	if tx, ok := txFrom(ctx); ok {
		return tx
	}
	return pool
}

// Inbox 實作 app.Inbox（包裝 pkg/outbox.Inbox，必須在交易內）。
type Inbox struct{ inbox *outbox.Inbox }

// MarkProcessed 實作 app.Inbox。
func (i *Inbox) MarkProcessed(ctx context.Context, eventID, consumer string) (bool, error) {
	tx, ok := txFrom(ctx)
	if !ok {
		return false, errors.New("postgres: inbox requires an active transaction")
	}
	return i.inbox.MarkProcessed(ctx, tx, eventID, consumer)
}

// Outbox 實作 app.OutboxStore（包裝 pkg/outbox.Store，必須在交易內）。
type Outbox struct{ store *outbox.Store }

// Insert 實作 app.OutboxStore。
func (o *Outbox) Insert(ctx context.Context, msg app.OutboxMessage) (string, error) {
	tx, ok := txFrom(ctx)
	if !ok {
		return "", errors.New("postgres: outbox requires an active transaction")
	}
	return o.store.Insert(ctx, tx, outbox.Message{
		AggregateType: msg.AggregateType, AggregateID: msg.AggregateID, EventType: msg.EventType,
		Payload: msg.Payload, Headers: msg.Headers,
	})
}

// optimistic 檢查 UPDATE ... WHERE version = $n 的結果。
func optimistic(tag pgconn.CommandTag, err error, what string) error {
	if err != nil {
		return fmt.Errorf("postgres: update %s: %w", what, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrConcurrentModification
	}
	return nil
}

// 確保介面實作。
var (
	_ app.TxManager   = (*TxManager)(nil)
	_ app.Inbox       = (*Inbox)(nil)
	_ app.OutboxStore = (*Outbox)(nil)
)

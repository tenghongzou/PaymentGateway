package postgres

import (
	"context"
	"errors"

	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
)

// OutboxStore 實作 app.OutboxStore：包裝 pkg/outbox.Store，從 ctx 取得交易。
type OutboxStore struct {
	store *outbox.Store
}

// NewOutboxStore 建立 OutboxStore。
func NewOutboxStore() *OutboxStore { return &OutboxStore{store: outbox.NewStore()} }

// ErrNoTransaction 表示 Insert 在交易外被呼叫（outbox 必須與業務資料同交易）。
var ErrNoTransaction = errors.New("postgres: outbox insert requires an active transaction")

// Insert 寫入 outbox（必須在 TxManager.WithinTx 內）。
func (o *OutboxStore) Insert(ctx context.Context, msg app.OutboxMessage) (string, error) {
	tx, ok := txFrom(ctx)
	if !ok {
		return "", ErrNoTransaction
	}
	return o.store.Insert(ctx, tx, outbox.Message{
		ID: msg.ID, AggregateType: msg.AggregateType, AggregateID: msg.AggregateID, EventType: msg.EventType, Payload: msg.Payload, Headers: msg.Headers,
	})
}

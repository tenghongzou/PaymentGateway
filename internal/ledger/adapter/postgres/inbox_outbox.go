package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/ledger/app"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
)

// errNoTx 表示需要交易的操作在交易外被呼叫。
var errNoTx = errors.New("ledger/postgres: operation requires a transaction (use TxRunner.RunInTx)")

// Inbox 以 pkg/outbox.Inbox 實作 app.Inbox（processed_events 去重）。
type Inbox struct {
	inbox *outbox.Inbox
}

// NewInbox 建立 Inbox。
func NewInbox() *Inbox { return &Inbox{inbox: outbox.NewInbox()} }

// MarkProcessed 在目前交易內記錄 (event_id, consumer)。
func (i *Inbox) MarkProcessed(ctx context.Context, eventID uuid.UUID, consumer string) (bool, error) {
	tx, ok := TxFromContext(ctx)
	if !ok {
		return false, errNoTx
	}
	return i.inbox.MarkProcessed(ctx, tx, eventID.String(), consumer)
}

// OutboxStore 以 pkg/outbox.Store 實作 app.OutboxStore。
type OutboxStore struct {
	store *outbox.Store
}

// NewOutboxStore 建立 OutboxStore。
func NewOutboxStore() *OutboxStore { return &OutboxStore{store: outbox.NewStore()} }

// Insert 在目前交易內寫入 outbox。
func (o *OutboxStore) Insert(ctx context.Context, msg app.OutboxMessage) (string, error) {
	tx, ok := TxFromContext(ctx)
	if !ok {
		return "", errNoTx
	}
	return o.store.Insert(ctx, tx, outbox.Message{
		AggregateType: msg.AggregateType,
		AggregateID:   msg.AggregateID,
		EventType:     msg.EventType,
		Payload:       msg.Payload,
		Headers:       msg.Headers,
	})
}

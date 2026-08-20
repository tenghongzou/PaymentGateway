package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/outbox"
)

// Inbox 以 pkg/outbox.Inbox 實作 app.Inbox（processed_events 去重）；必須在 Store.InTx 內呼叫。
type Inbox struct {
	inbox *outbox.Inbox
}

// NewInbox 建立 Inbox。
func NewInbox() *Inbox { return &Inbox{inbox: outbox.NewInbox()} }

// MarkProcessed 實作 app.Inbox。
func (i *Inbox) MarkProcessed(ctx context.Context, eventID uuid.UUID, consumer string) (bool, error) {
	tx, err := txFrom(ctx)
	if err != nil {
		return false, err
	}
	return i.inbox.MarkProcessed(ctx, tx, eventID.String(), consumer)
}

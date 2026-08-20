// Package outbox 實作 Transactional Outbox（docs/01 §6.2、docs/05 §8）。
//
//   - Store.Insert：與業務資料同一交易寫入 outbox 表。
//   - Relay：polling + FOR UPDATE SKIP LOCKED 取批次送到 Publisher（Kafka），成功標記 published_at。
//   - Inbox.MarkProcessed：消費端以 processed_events(event_id, consumer) 去重。
//
// 所有擁有 DB 的服務的 outbox / processed_events 表結構相同（見 migrations/<svc>/*_outbox.up.sql）。
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Message 為一筆待發佈的事件。
type Message struct {
	// ID 為 outbox.id（uuid 字串），同時是下游去重用的 event_id；空字串時 Insert 會自動產生 UUIDv7。
	ID            string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	Headers       map[string]string
	Attempts      int
}

// Publisher 為 relay 的輸出端（由 pkg/eventbus.Producer 實作）。
type Publisher interface {
	Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error
}

// Store 負責寫入 outbox 表（無狀態）。
type Store struct{}

// NewStore 建立 Store。
func NewStore() *Store { return &Store{} }

// Insert 在既有交易內寫入一筆訊息；回傳實際使用的 event id。
func (*Store) Insert(ctx context.Context, tx pgx.Tx, msg Message) (string, error) {
	if msg.ID == "" {
		u, err := uuid.NewV7()
		if err != nil {
			return "", fmt.Errorf("outbox: uuid: %w", err)
		}
		msg.ID = u.String()
	}
	if msg.Headers == nil {
		msg.Headers = map[string]string{}
	}
	headers, err := json.Marshal(msg.Headers)
	if err != nil {
		return "", fmt.Errorf("outbox: marshal headers: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (id, aggregate_type, aggregate_id, event_type, payload, headers)
		VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb)`,
		msg.ID, msg.AggregateType, msg.AggregateID, msg.EventType, msg.Payload, headers)
	if err != nil {
		return "", fmt.Errorf("outbox: insert: %w", err)
	}
	return msg.ID, nil
}

// Inbox 負責消費端去重。
type Inbox struct{}

// NewInbox 建立 Inbox。
func NewInbox() *Inbox { return &Inbox{} }

// MarkProcessed 在既有交易內記錄 (event_id, consumer)；若已存在回傳 already=true（呼叫端應略過該事件）。
func (*Inbox) MarkProcessed(ctx context.Context, tx pgx.Tx, eventID, consumer string) (already bool, err error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO processed_events (event_id, consumer) VALUES ($1::uuid, $2)
		ON CONFLICT DO NOTHING`, eventID, consumer)
	if err != nil {
		return false, fmt.Errorf("outbox: mark processed: %w", err)
	}
	return tag.RowsAffected() == 0, nil
}

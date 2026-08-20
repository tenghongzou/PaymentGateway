package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
)

// EventRepo 實作 app.EventRepo（webhook_events）。
type EventRepo struct {
	s *Store
}

// NewEventRepo 建立 EventRepo。
func NewEventRepo(s *Store) *EventRepo { return &EventRepo{s: s} }

// Insert 寫入事件；event_id 已存在時略過（冪等）。
func (r *EventRepo) Insert(ctx context.Context, ev *domain.Event) error {
	_, err := r.s.q(ctx).Exec(ctx, `
		INSERT INTO webhook_events (event_id, merchant_id, event_type, resource_type, resource_id, payload, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
		ON CONFLICT (event_id) DO NOTHING`,
		ev.ID, ev.MerchantID, ev.Type, ev.ResourceType, ev.ResourceID, ev.Payload, ev.OccurredAt)
	if err != nil {
		return fmt.Errorf("postgres: insert webhook_event: %w", err)
	}
	return nil
}

// Get 取得事件。
func (r *EventRepo) Get(ctx context.Context, merchantID, eventID uuid.UUID) (*domain.Event, error) {
	row := r.s.q(ctx).QueryRow(ctx, `
		SELECT event_id, merchant_id, event_type, resource_type, resource_id, payload, occurred_at, created_at,
		       COALESCE((payload->>'livemode')::boolean, false), COALESCE(payload->>'payment_id', '')
		  FROM webhook_events WHERE event_id = $1 AND merchant_id = $2`, eventID, merchantID)
	var ev domain.Event
	err := row.Scan(&ev.ID, &ev.MerchantID, &ev.Type, &ev.ResourceType, &ev.ResourceID, &ev.Payload, &ev.OccurredAt, &ev.CreatedAt, &ev.Livemode, &ev.PaymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEventNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get webhook_event: %w", err)
	}
	return &ev, nil
}

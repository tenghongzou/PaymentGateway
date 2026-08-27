package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// DeliveryRepo 實作 app.DeliveryRepo（webhook_deliveries / webhook_delivery_attempts）。
type DeliveryRepo struct {
	s *Store
}

// NewDeliveryRepo 建立 DeliveryRepo。
func NewDeliveryRepo(s *Store) *DeliveryRepo { return &DeliveryRepo{s: s} }

// deliveryColumns 為 webhook_deliveries 欄位 + webhook_events 投影欄位（別名 d / e）。
const deliveryColumns = `
	d.id, d.event_id, d.endpoint_id, d.merchant_id, d.attempt_no, d.status, d.next_attempt_at, d.last_attempt_at,
	d.last_response_status, d.last_response_body, d.last_error, d.delivered_at, d.created_at, d.updated_at, d.version,
	e.event_type, e.payload, COALESCE((e.payload->>'livemode')::boolean, false), e.occurred_at`

func scanDelivery(row pgx.Row) (*domain.Delivery, error) {
	var d domain.Delivery
	var status string
	err := row.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.MerchantID, &d.AttemptNo, &status, &d.NextAttemptAt, &d.LastAttemptAt,
		&d.LastResponseStatus, &d.LastResponseBody, &d.LastError, &d.DeliveredAt, &d.CreatedAt, &d.UpdatedAt, &d.Version,
		&d.EventType, &d.EventPayload, &d.Livemode, &d.OccurredAt)
	if err != nil {
		return nil, err
	}
	d.Status = domain.DeliveryStatus(status)
	return &d, nil
}

func scanDeliveries(rows pgx.Rows) ([]*domain.Delivery, error) {
	defer rows.Close()
	var out []*domain.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// InsertPending 批次寫入 pending deliveries（ON CONFLICT (event_id, endpoint_id) DO NOTHING）。
func (r *DeliveryRepo) InsertPending(ctx context.Context, ds []*domain.Delivery) error {
	if len(ds) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, d := range ds {
		batch.Queue(`
			INSERT INTO webhook_deliveries (id, event_id, endpoint_id, merchant_id, attempt_no, status, next_attempt_at, created_at, updated_at, version)
			VALUES ($1, $2, $3, $4, 0, 'pending', $5, $6, $6, 0)
			ON CONFLICT ON CONSTRAINT webhook_deliveries_event_endpoint_key DO NOTHING`,
			d.ID, d.EventID, d.EndpointID, d.MerchantID, d.NextAttemptAt, d.CreatedAt)
	}
	sb, ok := r.s.q(ctx).(interface {
		SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	})
	if !ok {
		// pgxpool.Pool 與 pgx.Tx 都支援 SendBatch；此分支僅防禦 querier 實作被抽換。
		return errors.New("postgres: querier does not support SendBatch")
	}
	br := sb.SendBatch(ctx, batch)
	defer br.Close()
	for range ds {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("postgres: insert webhook_delivery: %w", err)
		}
	}
	return nil
}

// ClaimDue 取件（migrations/webhook/0001 註解的 UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED) RETURNING）。
func (r *DeliveryRepo) ClaimDue(ctx context.Context, now time.Time, limit int) ([]*domain.Delivery, error) {
	rows, err := r.s.q(ctx).Query(ctx, `
		WITH claimed AS (
			UPDATE webhook_deliveries
			   SET status = 'in_flight', attempt_no = attempt_no + 1, version = version + 1, updated_at = $1
			 WHERE id IN (SELECT id FROM webhook_deliveries
			               WHERE status IN ('pending', 'failed') AND next_attempt_at <= $1
			               ORDER BY next_attempt_at
			               FOR UPDATE SKIP LOCKED
			               LIMIT $2)
			RETURNING *
		)
		SELECT `+deliveryColumns+`
		  FROM claimed d JOIN webhook_events e ON e.event_id = d.event_id
		 ORDER BY d.next_attempt_at`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: claim deliveries: %w", err)
	}
	ds, err := scanDeliveries(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: scan claimed deliveries: %w", err)
	}
	return ds, nil
}

// Save 以樂觀鎖寫回狀態；att 非 nil 時同交易寫入 attempt。
func (r *DeliveryRepo) Save(ctx context.Context, d *domain.Delivery, att *domain.Attempt) error {
	return r.s.InTx(ctx, func(ctx context.Context) error {
		q := r.s.q(ctx)
		tag, err := q.Exec(ctx, `
			UPDATE webhook_deliveries
			   SET status = $2, attempt_no = $3, next_attempt_at = $4, last_attempt_at = $5, last_response_status = $6,
			       last_response_body = $7, last_error = $8, delivered_at = $9, version = $10, updated_at = $11
			 WHERE id = $1 AND version = $10 - 1`,
			d.ID, string(d.Status), d.AttemptNo, d.NextAttemptAt, d.LastAttemptAt, d.LastResponseStatus,
			d.LastResponseBody, d.LastError, d.DeliveredAt, d.Version, d.UpdatedAt)
		if err != nil {
			return fmt.Errorf("postgres: update webhook_delivery: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("postgres: update webhook_delivery %s: %w", d.ID, pgdb.ErrConcurrentModification)
		}
		if att == nil {
			return nil
		}
		_, err = q.Exec(ctx, `
			INSERT INTO webhook_delivery_attempts (id, delivery_id, attempt_no, response_status, response_body, error, duration_ms, attempted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT ON CONSTRAINT webhook_delivery_attempts_delivery_no_key DO NOTHING`,
			att.ID, att.DeliveryID, att.AttemptNo, att.ResponseStatus, att.ResponseBody, att.Error, att.DurationMS, att.AttemptedAt)
		if err != nil {
			return fmt.Errorf("postgres: insert webhook_delivery_attempt: %w", err)
		}
		return nil
	})
}

// Get 取得 delivery（含事件投影）。
func (r *DeliveryRepo) Get(ctx context.Context, merchantID, id uuid.UUID) (*domain.Delivery, error) {
	row := r.s.q(ctx).QueryRow(ctx, `
		SELECT `+deliveryColumns+`
		  FROM webhook_deliveries d JOIN webhook_events e ON e.event_id = d.event_id
		 WHERE d.id = $1 AND d.merchant_id = $2`, id, merchantID)
	d, err := scanDelivery(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDeliveryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get webhook_delivery: %w", err)
	}
	return d, nil
}

// ListAttempts 依 attempt_no 升冪列出。
func (r *DeliveryRepo) ListAttempts(ctx context.Context, deliveryID uuid.UUID) ([]*domain.Attempt, error) {
	rows, err := r.s.q(ctx).Query(ctx, `
		SELECT id, delivery_id, attempt_no, response_status, response_body, error, duration_ms, attempted_at
		  FROM webhook_delivery_attempts WHERE delivery_id = $1 ORDER BY attempt_no`, deliveryID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list attempts: %w", err)
	}
	defer rows.Close()
	var out []*domain.Attempt
	for rows.Next() {
		var a domain.Attempt
		var dur *int
		if err := rows.Scan(&a.ID, &a.DeliveryID, &a.AttemptNo, &a.ResponseStatus, &a.ResponseBody, &a.Error, &dur, &a.AttemptedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan attempt: %w", err)
		}
		if dur != nil {
			a.DurationMS = *dur
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// List 以 (created_at, id) keyset 分頁列出（created_at DESC）。
func (r *DeliveryRepo) List(ctx context.Context, f app.DeliveryFilter) (*app.DeliveryPage, error) {
	size := f.PageSize
	if size <= 0 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	where = append(where, "d.merchant_id = "+arg(f.MerchantID))
	if f.EndpointID != nil {
		where = append(where, "d.endpoint_id = "+arg(*f.EndpointID))
	}
	if f.EventID != nil {
		where = append(where, "d.event_id = "+arg(*f.EventID))
	}
	if f.EventType != "" {
		where = append(where, "e.event_type = "+arg(f.EventType))
	}
	if len(f.Statuses) > 0 {
		ss := make([]string, 0, len(f.Statuses))
		for _, s := range f.Statuses {
			ss = append(ss, string(s))
		}
		where = append(where, "d.status = ANY("+arg(ss)+")")
	}
	if f.CreatedAfter != nil {
		where = append(where, "d.created_at >= "+arg(*f.CreatedAfter))
	}
	if f.CreatedBefore != nil {
		where = append(where, "d.created_at <= "+arg(*f.CreatedBefore))
	}
	if f.Livemode != nil {
		where = append(where, "COALESCE((e.payload->>'livemode')::boolean, false) = "+arg(*f.Livemode))
	}
	if f.PageToken != "" {
		ts, id, err := decodePageToken(f.PageToken)
		if err != nil {
			return nil, err
		}
		where = append(where, "(d.created_at, d.id) < ("+arg(ts)+", "+arg(id)+")")
	}
	sql := `SELECT ` + deliveryColumns + `
		  FROM webhook_deliveries d JOIN webhook_events e ON e.event_id = d.event_id
		 WHERE ` + strings.Join(where, " AND ") + `
		 ORDER BY d.created_at DESC, d.id DESC
		 LIMIT ` + arg(size+1)
	rows, err := r.s.q(ctx).Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list deliveries: %w", err)
	}
	ds, err := scanDeliveries(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: scan deliveries: %w", err)
	}
	page := &app.DeliveryPage{Deliveries: ds}
	if len(ds) > size {
		page.Deliveries = ds[:size]
		last := page.Deliveries[size-1]
		page.NextPageToken = encodePageToken(last.CreatedAt, last.ID)
	}
	return page, nil
}

// ReapStuck 回收卡住的 in_flight（updated_at < before）。attempt_no 已達上限者直接進死信。
func (r *DeliveryRepo) ReapStuck(ctx context.Context, before, now time.Time) (int64, error) {
	tag, err := r.s.q(ctx).Exec(ctx, `
		UPDATE webhook_deliveries
		   SET status = CASE WHEN attempt_no >= $3 THEN 'dead_letter' ELSE 'failed' END,
		       next_attempt_at = $2, last_error = 'in_flight timeout; reclaimed by reaper',
		       version = version + 1, updated_at = $2
		 WHERE status = 'in_flight' AND updated_at < $1`, before, now, domain.MaxAttempts)
	if err != nil {
		return 0, fmt.Errorf("postgres: reap stuck deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CancelForEndpoint 取消端點所有非終態 delivery。
func (r *DeliveryRepo) CancelForEndpoint(ctx context.Context, endpointID uuid.UUID, now time.Time, reason string) (int64, error) {
	tag, err := r.s.q(ctx).Exec(ctx, `
		UPDATE webhook_deliveries
		   SET status = 'canceled', last_error = $3, version = version + 1, updated_at = $2
		 WHERE endpoint_id = $1 AND status IN ('pending', 'failed', 'in_flight')`, endpointID, now, reason)
	if err != nil {
		return 0, fmt.Errorf("postgres: cancel deliveries for endpoint: %w", err)
	}
	return tag.RowsAffected(), nil
}

func encodePageToken(ts time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ts.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodePageToken(tok string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return time.Time{}, uuid.Nil, app.ErrInvalidPageToken
	}
	tsStr, idStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, app.ErrInvalidPageToken
	}
	ts, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return time.Time{}, uuid.Nil, app.ErrInvalidPageToken
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return time.Time{}, uuid.Nil, app.ErrInvalidPageToken
	}
	return ts, id, nil
}

var (
	_ app.DeliveryRepo = (*DeliveryRepo)(nil)
	_ app.EventRepo    = (*EventRepo)(nil)
	_ app.Inbox        = (*Inbox)(nil)
	_ app.Transactor   = (*Store)(nil)
)

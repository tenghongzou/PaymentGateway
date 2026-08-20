package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// WebhookEndpointRepo 實作 app.WebhookEndpointRepo（表 webhook_endpoints）。
type WebhookEndpointRepo struct {
	pool *pgxpool.Pool
}

// NewWebhookEndpointRepo 建立 WebhookEndpointRepo。
func NewWebhookEndpointRepo(pool *pgxpool.Pool) *WebhookEndpointRepo {
	return &WebhookEndpointRepo{pool: pool}
}

// metadata jsonb 內部鍵（DB 無 mode / deleted_at / auto_disabled 專欄；TODO 補 migration）。
const (
	metaMode         = "_mode"
	metaDeletedAt    = "_deleted_at"
	metaAutoDisabled = "_auto_disabled"
)

const webhookColumns = `id, merchant_id, url, description, secret_current, secret_previous, secret_rotated_at, enabled_events, status, metadata, created_at, updated_at, version`

func webhookMetaJSON(e *domain.WebhookEndpoint) ([]byte, error) {
	m := make(map[string]any, len(e.Metadata)+3)
	for k, v := range e.Metadata {
		m[k] = v
	}
	m[metaMode] = string(e.Mode)
	if e.DeletedAt != nil {
		m[metaDeletedAt] = e.DeletedAt.UTC().Format(time.RFC3339Nano)
	}
	if e.AutoDisabled {
		m[metaAutoDisabled] = true
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("postgres: marshal endpoint metadata: %w", err)
	}
	return b, nil
}

// Create 新增端點。
func (r *WebhookEndpointRepo) Create(ctx context.Context, e *domain.WebhookEndpoint) error {
	meta, err := webhookMetaJSON(e)
	if err != nil {
		return err
	}
	_, err = dbFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO webhook_endpoints (id, public_id, merchant_id, url, description, secret_current, secret_previous, secret_rotated_at,
		                               enabled_events, status, metadata, created_at, updated_at, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12, $13, $14)`,
		e.ID, e.PublicID(), e.MerchantID, e.URL, nullIfEmpty(e.Description), e.SecretCurrentEnc, nullIfEmpty(e.SecretPreviousEnc), e.SecretRotatedAt,
		nonNilSlice(e.EnabledEvents), string(e.Status), meta, e.CreatedAt, e.UpdatedAt, e.Version)
	return mapErr("insert endpoint", err)
}

// Get 依商戶 + id 取得端點。
func (r *WebhookEndpointRepo) Get(ctx context.Context, merchantID, id uuid.UUID) (*domain.WebhookEndpoint, error) {
	row := dbFrom(ctx, r.pool).QueryRow(ctx, `SELECT `+webhookColumns+` FROM webhook_endpoints WHERE id = $1 AND merchant_id = $2`, id, merchantID)
	return scanEndpoint(row)
}

// Update 以 version 做樂觀鎖更新。
func (r *WebhookEndpointRepo) Update(ctx context.Context, e *domain.WebhookEndpoint) error {
	meta, err := webhookMetaJSON(e)
	if err != nil {
		return err
	}
	var version int
	var updatedAt time.Time
	err = dbFrom(ctx, r.pool).QueryRow(ctx, `
		UPDATE webhook_endpoints
		   SET url = $3, description = $4, secret_current = $5, secret_previous = $6, secret_rotated_at = $7,
		       enabled_events = $8, status = $9, metadata = $10::jsonb, version = version + 1
		 WHERE id = $1 AND merchant_id = $2 AND version = $11
		RETURNING version, updated_at`,
		e.ID, e.MerchantID, e.URL, nullIfEmpty(e.Description), e.SecretCurrentEnc, nullIfEmpty(e.SecretPreviousEnc), e.SecretRotatedAt,
		nonNilSlice(e.EnabledEvents), string(e.Status), meta, e.Version).Scan(&version, &updatedAt)
	if err != nil {
		if err == pgx.ErrNoRows { //nolint:errorlint // sentinel
			var exists bool
			if qerr := dbFrom(ctx, r.pool).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM webhook_endpoints WHERE id = $1 AND merchant_id = $2)`, e.ID, e.MerchantID).Scan(&exists); qerr == nil && !exists {
				return domain.ErrNotFound
			}
			return domain.ErrConcurrentModify
		}
		return mapErr("update endpoint", err)
	}
	e.Version = version
	e.UpdatedAt = updatedAt
	return nil
}

// CountLive 計算未刪除端點數。
func (r *WebhookEndpointRepo) CountLive(ctx context.Context, merchantID uuid.UUID) (int, error) {
	var n int
	err := dbFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM webhook_endpoints WHERE merchant_id = $1 AND NOT (metadata ? '`+metaDeletedAt+`')`, merchantID).Scan(&n)
	return n, mapErr("count endpoints", err)
}

// List 列出端點（keyset 分頁）。
func (r *WebhookEndpointRepo) List(ctx context.Context, merchantID uuid.UUID, f app.WebhookEndpointFilter, p app.Page) ([]*domain.WebhookEndpoint, string, error) {
	p = p.Normalize()
	fh := filterHash("webhook_endpoints", merchantID.String(), string(f.Mode), strconv.FormatBool(f.IncludeDeleted))
	cur, err := decodeCursor(p.Token, fh)
	if err != nil {
		return nil, "", err
	}
	q := &query{}
	q.add("merchant_id = $1", merchantID)
	if f.Mode != "" {
		q.add("metadata->>'"+metaMode+"' = $"+strconv.Itoa(q.next()), string(f.Mode))
	}
	if !f.IncludeDeleted {
		q.add("NOT (metadata ? '" + metaDeletedAt + "')")
	}
	if cur != nil {
		n := q.next()
		q.add(fmt.Sprintf("(created_at, id) < ($%d, $%d)", n, n+1), cur.CreatedAt, cur.ID)
	}
	q.args = append(q.args, p.Size+1)
	rows, err := dbFrom(ctx, r.pool).Query(ctx,
		`SELECT `+webhookColumns+` FROM webhook_endpoints`+q.sql()+` ORDER BY created_at DESC, id DESC LIMIT $`+strconv.Itoa(len(q.args)), q.args...)
	if err != nil {
		return nil, "", mapErr("list endpoints", err)
	}
	defer rows.Close()
	var out []*domain.WebhookEndpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapErr("endpoint rows", err)
	}
	next := ""
	if len(out) > p.Size {
		out = out[:p.Size]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID, fh)
	}
	return out, next, nil
}

func scanEndpoint(row pgx.Row) (*domain.WebhookEndpoint, error) {
	var (
		e           domain.WebhookEndpoint
		description *string
		secretPrev  *string
		status      string
		meta        []byte
	)
	err := row.Scan(&e.ID, &e.MerchantID, &e.URL, &description, &e.SecretCurrentEnc, &secretPrev, &e.SecretRotatedAt, &e.EnabledEvents, &status, &meta, &e.CreatedAt, &e.UpdatedAt, &e.Version)
	if err != nil {
		return nil, mapErr("scan endpoint", err)
	}
	if description != nil {
		e.Description = *description
	}
	if secretPrev != nil {
		e.SecretPreviousEnc = *secretPrev
	}
	e.Status = domain.EndpointStatus(status)
	if e.EnabledEvents == nil {
		e.EnabledEvents = []string{}
	}
	e.Metadata = map[string]string{}
	if len(meta) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(meta, &raw); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal endpoint metadata: %w", err)
		}
		for k, v := range raw {
			switch {
			case k == metaMode:
				if s, ok := v.(string); ok {
					e.Mode = domain.Mode(s)
				}
			case k == metaDeletedAt:
				if s, ok := v.(string); ok {
					if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
						e.DeletedAt = &t
					}
				}
			case k == metaAutoDisabled:
				if b, ok := v.(bool); ok {
					e.AutoDisabled = b
				}
			case strings.HasPrefix(k, "_"):
				// 未知內部鍵：忽略
			default:
				if s, ok := v.(string); ok {
					e.Metadata[k] = s
				}
			}
		}
	}
	return &e, nil
}

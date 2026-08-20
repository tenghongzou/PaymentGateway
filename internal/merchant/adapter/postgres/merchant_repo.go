package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// MerchantRepo 實作 app.MerchantRepo（表 merchants）。
type MerchantRepo struct {
	pool *pgxpool.Pool
}

// NewMerchantRepo 建立 MerchantRepo。
func NewMerchantRepo(pool *pgxpool.Pool) *MerchantRepo { return &MerchantRepo{pool: pool} }

const merchantColumns = `id, name, status, country, default_currency, settings, metadata, created_at, updated_at, version`

// Create 新增商戶。
func (r *MerchantRepo) Create(ctx context.Context, m *domain.Merchant) error {
	settings, err := json.Marshal(m.Settings)
	if err != nil {
		return fmt.Errorf("postgres: marshal settings: %w", err)
	}
	metadata, err := json.Marshal(nonNil(m.Metadata))
	if err != nil {
		return fmt.Errorf("postgres: marshal metadata: %w", err)
	}
	_, err = dbFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO merchants (id, public_id, name, status, country, default_currency, settings, metadata, created_at, updated_at, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9, $10, $11)`,
		m.ID, m.PublicID(), m.Name, string(m.Status), m.Country, m.DefaultCurrency, settings, metadata, m.CreatedAt, m.UpdatedAt, m.Version)
	return mapErr("insert merchant", err)
}

// Get 依 id 取得商戶。
func (r *MerchantRepo) Get(ctx context.Context, id uuid.UUID) (*domain.Merchant, error) {
	row := dbFrom(ctx, r.pool).QueryRow(ctx, `SELECT `+merchantColumns+` FROM merchants WHERE id = $1`, id)
	return scanMerchant(row)
}

// GetForUpdate 以 row lock 取得商戶（需在交易內）。
func (r *MerchantRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*domain.Merchant, error) {
	row := dbFrom(ctx, r.pool).QueryRow(ctx, `SELECT `+merchantColumns+` FROM merchants WHERE id = $1 FOR UPDATE`, id)
	return scanMerchant(row)
}

// FindByExternalRef 以 settings->>'external_ref' 查詢。
func (r *MerchantRepo) FindByExternalRef(ctx context.Context, ref string) (*domain.Merchant, error) {
	row := dbFrom(ctx, r.pool).QueryRow(ctx, `SELECT `+merchantColumns+` FROM merchants WHERE settings->>'external_ref' = $1 ORDER BY created_at LIMIT 1`, ref)
	return scanMerchant(row)
}

// Update 以 version 做樂觀鎖更新；updated_at 由 DB trigger 維護並回填。
func (r *MerchantRepo) Update(ctx context.Context, m *domain.Merchant) error {
	settings, err := json.Marshal(m.Settings)
	if err != nil {
		return fmt.Errorf("postgres: marshal settings: %w", err)
	}
	metadata, err := json.Marshal(nonNil(m.Metadata))
	if err != nil {
		return fmt.Errorf("postgres: marshal metadata: %w", err)
	}
	var version int
	var updatedAt time.Time
	err = dbFrom(ctx, r.pool).QueryRow(ctx, `
		UPDATE merchants
		   SET name = $2, status = $3, country = $4, default_currency = $5, settings = $6::jsonb, metadata = $7::jsonb, version = version + 1
		 WHERE id = $1 AND version = $8
		RETURNING version, updated_at`,
		m.ID, m.Name, string(m.Status), m.Country, m.DefaultCurrency, settings, metadata, m.Version).Scan(&version, &updatedAt)
	if err != nil {
		if err == pgx.ErrNoRows { //nolint:errorlint // pgx.ErrNoRows 為 sentinel
			// 不存在或版本不符：區分兩者
			var exists bool
			if qerr := dbFrom(ctx, r.pool).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM merchants WHERE id = $1)`, m.ID).Scan(&exists); qerr == nil && !exists {
				return domain.ErrNotFound
			}
			return domain.ErrConcurrentModify
		}
		return mapErr("update merchant", err)
	}
	m.Version = version
	m.UpdatedAt = updatedAt
	return nil
}

// List 依 created_at DESC, id DESC keyset 分頁。
func (r *MerchantRepo) List(ctx context.Context, f app.MerchantFilter, p app.Page) ([]*domain.Merchant, string, error) {
	p = p.Normalize()
	fh := filterHash("merchants", string(f.Status), f.Country)
	cur, err := decodeCursor(p.Token, fh)
	if err != nil {
		return nil, "", err
	}
	q := &query{}
	if f.Status != "" {
		q.add("status = $"+strconv.Itoa(q.next()), string(f.Status))
	}
	if f.Country != "" {
		q.add("country = $"+strconv.Itoa(q.next()), f.Country)
	}
	if cur != nil {
		n := q.next()
		q.add(fmt.Sprintf("(created_at, id) < ($%d, $%d)", n, n+1), cur.CreatedAt, cur.ID)
	}
	q.args = append(q.args, p.Size+1)
	rows, err := dbFrom(ctx, r.pool).Query(ctx,
		`SELECT `+merchantColumns+` FROM merchants`+q.sql()+` ORDER BY created_at DESC, id DESC LIMIT $`+strconv.Itoa(len(q.args)), q.args...)
	if err != nil {
		return nil, "", mapErr("list merchants", err)
	}
	defer rows.Close()
	var out []*domain.Merchant
	for rows.Next() {
		m, err := scanMerchant(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapErr("list merchants rows", err)
	}
	next := ""
	if len(out) > p.Size {
		out = out[:p.Size]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID, fh)
	}
	return out, next, nil
}

func scanMerchant(row pgx.Row) (*domain.Merchant, error) {
	var (
		m        domain.Merchant
		status   string
		settings []byte
		metadata []byte
	)
	err := row.Scan(&m.ID, &m.Name, &status, &m.Country, &m.DefaultCurrency, &settings, &metadata, &m.CreatedAt, &m.UpdatedAt, &m.Version)
	if err != nil {
		return nil, mapErr("scan merchant", err)
	}
	m.Status = domain.Status(status)
	if len(settings) > 0 {
		if err := json.Unmarshal(settings, &m.Settings); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal settings: %w", err)
		}
	}
	m.Metadata = map[string]string{}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &m.Metadata); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal metadata: %w", err)
		}
	}
	return &m, nil
}

func nonNil(md map[string]string) map[string]string {
	if md == nil {
		return map[string]string{}
	}
	return md
}

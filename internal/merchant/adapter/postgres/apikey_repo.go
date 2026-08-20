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

// ApiKeyRepo 實作 app.ApiKeyRepo（表 api_keys）。
type ApiKeyRepo struct { //nolint:revive // 與 proto 命名一致
	pool *pgxpool.Pool
}

// NewApiKeyRepo 建立 ApiKeyRepo。
func NewApiKeyRepo(pool *pgxpool.Pool) *ApiKeyRepo { return &ApiKeyRepo{pool: pool} } //nolint:revive // 與 proto 命名一致

// apiKeyMeta 為 api_keys.metadata jsonb 的內部鍵（DB 無 signing secret 專欄的 Phase 0 權宜；TODO 補 migration）。
type apiKeyMeta struct {
	SigningSecretEnc         string     `json:"_signing_secret_enc,omitempty"`
	PreviousSigningSecretEnc string     `json:"_previous_signing_secret_enc,omitempty"`
	PreviousSecretExpiresAt  *time.Time `json:"_previous_secret_expires_at,omitempty"`
}

const apiKeyColumns = `id, merchant_id, prefix, key_hash, mode, name, scopes, last_used_at, expires_at, revoked_at, metadata, created_at, updated_at`

func apiKeyMetaJSON(k *domain.ApiKey) ([]byte, error) {
	b, err := json.Marshal(apiKeyMeta{
		SigningSecretEnc: k.SigningSecretEnc, PreviousSigningSecretEnc: k.PreviousSigningSecretEnc, PreviousSecretExpiresAt: k.PreviousSecretExpiresAt,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: marshal api key metadata: %w", err)
	}
	return b, nil
}

// Create 新增 key。
func (r *ApiKeyRepo) Create(ctx context.Context, k *domain.ApiKey) error {
	meta, err := apiKeyMetaJSON(k)
	if err != nil {
		return err
	}
	_, err = dbFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO api_keys (id, merchant_id, prefix, key_hash, mode, name, scopes, expires_at, revoked_at, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12)`,
		k.ID, k.MerchantID, k.Prefix, k.KeyHash, string(k.Mode), nullIfEmpty(k.Name), nonNilSlice(k.Scopes), k.ExpiresAt, k.RevokedAt, meta, k.CreatedAt, k.UpdatedAt)
	return mapErr("insert api key", err)
}

// Get 依商戶 + id 取得 key。
func (r *ApiKeyRepo) Get(ctx context.Context, merchantID, id uuid.UUID) (*domain.ApiKey, error) {
	row := dbFrom(ctx, r.pool).QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id = $1 AND merchant_id = $2`, id, merchantID)
	return scanAPIKey(row)
}

// FindByPrefix 以 prefix 查候選（UNIQUE，最多 1；LIMIT 2 保留 docs/06 的「最多 2 把」語意）。
func (r *ApiKeyRepo) FindByPrefix(ctx context.Context, prefix string) ([]*domain.ApiKey, error) {
	rows, err := dbFrom(ctx, r.pool).Query(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE prefix = $1 ORDER BY created_at DESC LIMIT 2`, prefix)
	if err != nil {
		return nil, mapErr("find api key by prefix", err)
	}
	defer rows.Close()
	return collectAPIKeys(rows)
}

// Update 更新可變欄位（name / scopes / expires_at / revoked_at / metadata）。
func (r *ApiKeyRepo) Update(ctx context.Context, k *domain.ApiKey) error {
	meta, err := apiKeyMetaJSON(k)
	if err != nil {
		return err
	}
	err = dbFrom(ctx, r.pool).QueryRow(ctx, `
		UPDATE api_keys
		   SET name = $3, scopes = $4, expires_at = $5, revoked_at = $6, metadata = $7::jsonb
		 WHERE id = $1 AND merchant_id = $2
		RETURNING updated_at`,
		k.ID, k.MerchantID, nullIfEmpty(k.Name), nonNilSlice(k.Scopes), k.ExpiresAt, k.RevokedAt, meta).Scan(&k.UpdatedAt)
	return mapErr("update api key", err)
}

// CountActive 計算未撤銷且未過期的 key 數。
func (r *ApiKeyRepo) CountActive(ctx context.Context, merchantID uuid.UUID, mode domain.Mode, now time.Time) (int, error) {
	var n int
	err := dbFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT count(*) FROM api_keys
		 WHERE merchant_id = $1 AND mode = $2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $3)`,
		merchantID, string(mode), now).Scan(&n)
	return n, mapErr("count api keys", err)
}

// List 列出商戶的 key（keyset 分頁）。
func (r *ApiKeyRepo) List(ctx context.Context, merchantID uuid.UUID, f app.ApiKeyFilter, p app.Page) ([]*domain.ApiKey, string, error) {
	p = p.Normalize()
	fh := filterHash("api_keys", merchantID.String(), string(f.Mode), strconv.FormatBool(f.IncludeInactive))
	cur, err := decodeCursor(p.Token, fh)
	if err != nil {
		return nil, "", err
	}
	q := &query{}
	q.add("merchant_id = $1", merchantID)
	if f.Mode != "" {
		q.add("mode = $"+strconv.Itoa(q.next()), string(f.Mode))
	}
	if !f.IncludeInactive {
		q.add("revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())")
	}
	if cur != nil {
		n := q.next()
		q.add(fmt.Sprintf("(created_at, id) < ($%d, $%d)", n, n+1), cur.CreatedAt, cur.ID)
	}
	q.args = append(q.args, p.Size+1)
	rows, err := dbFrom(ctx, r.pool).Query(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys`+q.sql()+` ORDER BY created_at DESC, id DESC LIMIT $`+strconv.Itoa(len(q.args)), q.args...)
	if err != nil {
		return nil, "", mapErr("list api keys", err)
	}
	defer rows.Close()
	out, err := collectAPIKeys(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > p.Size {
		out = out[:p.Size]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID, fh)
	}
	return out, next, nil
}

// TouchLastUsed 只在值往前時更新（避免亂序的非同步更新倒退）。
func (r *ApiKeyRepo) TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := dbFrom(ctx, r.pool).Exec(ctx,
		`UPDATE api_keys SET last_used_at = $2 WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2)`, id, at)
	return mapErr("touch api key", err)
}

func collectAPIKeys(rows pgx.Rows) ([]*domain.ApiKey, error) {
	var out []*domain.ApiKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr("api key rows", err)
	}
	return out, nil
}

func scanAPIKey(row pgx.Row) (*domain.ApiKey, error) {
	var (
		k    domain.ApiKey
		mode string
		name *string
		meta []byte
	)
	err := row.Scan(&k.ID, &k.MerchantID, &k.Prefix, &k.KeyHash, &mode, &name, &k.Scopes, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt, &meta, &k.CreatedAt, &k.UpdatedAt)
	if err != nil {
		return nil, mapErr("scan api key", err)
	}
	k.Mode = domain.Mode(mode)
	if name != nil {
		k.Name = *name
	}
	if k.Scopes == nil {
		k.Scopes = []string{}
	}
	if len(meta) > 0 {
		var m apiKeyMeta
		if err := json.Unmarshal(meta, &m); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal api key metadata: %w", err)
		}
		k.SigningSecretEnc = m.SigningSecretEnc
		k.PreviousSigningSecretEnc = m.PreviousSigningSecretEnc
		k.PreviousSecretExpiresAt = m.PreviousSecretExpiresAt
	}
	return &k, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

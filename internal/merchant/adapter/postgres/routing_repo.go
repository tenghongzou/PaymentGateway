package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// RoutingPrefRepo 實作 app.RoutingPrefRepo（表 routing_preferences，只負責 rules / version / updated_at）。
type RoutingPrefRepo struct {
	pool *pgxpool.Pool
}

// NewRoutingPrefRepo 建立 RoutingPrefRepo。
func NewRoutingPrefRepo(pool *pgxpool.Pool) *RoutingPrefRepo { return &RoutingPrefRepo{pool: pool} }

// Get 取得 rules；找不到回 domain.ErrNotFound。
func (r *RoutingPrefRepo) Get(ctx context.Context, merchantID uuid.UUID) (*domain.RoutingPreferences, error) {
	var (
		rules     []byte
		updatedAt time.Time
		version   int
	)
	err := dbFrom(ctx, r.pool).QueryRow(ctx, `SELECT rules, updated_at, version FROM routing_preferences WHERE merchant_id = $1`, merchantID).
		Scan(&rules, &updatedAt, &version)
	if err != nil {
		return nil, mapErr("get routing preferences", err)
	}
	p := &domain.RoutingPreferences{MerchantID: merchantID, UpdatedAt: &updatedAt, Version: version, Rules: []domain.RoutingRule{}}
	if len(rules) > 0 {
		if err := json.Unmarshal(rules, &p.Rules); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal routing rules: %w", err)
		}
	}
	if p.Rules == nil {
		p.Rules = []domain.RoutingRule{}
	}
	return p, nil
}

// Upsert 整份覆寫 rules；既有列 version + 1（trigger 維護 updated_at）。
func (r *RoutingPrefRepo) Upsert(ctx context.Context, p *domain.RoutingPreferences) error {
	rules := p.Rules
	if rules == nil {
		rules = []domain.RoutingRule{}
	}
	b, err := json.Marshal(rules)
	if err != nil {
		return fmt.Errorf("postgres: marshal routing rules: %w", err)
	}
	var updatedAt time.Time
	err = dbFrom(ctx, r.pool).QueryRow(ctx, `
		INSERT INTO routing_preferences (merchant_id, rules, version)
		VALUES ($1, $2::jsonb, 0)
		ON CONFLICT (merchant_id) DO UPDATE SET rules = EXCLUDED.rules, version = routing_preferences.version + 1
		RETURNING version, updated_at`, p.MerchantID, b).Scan(&p.Version, &updatedAt)
	if err != nil {
		return mapErr("upsert routing preferences", err)
	}
	p.UpdatedAt = &updatedAt
	return nil
}

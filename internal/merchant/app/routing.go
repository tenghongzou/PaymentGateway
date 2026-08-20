package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// GetRoutingPreferences 取得路由偏好；商戶未設定時回系統預設（rules 空、failover true、max_attempts 3）。
func (s *Service) GetRoutingPreferences(ctx context.Context, merchantID string) (*domain.RoutingPreferences, error) {
	m, err := s.loadMerchant(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	prefs, err := s.routing.Get(ctx, m.ID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("app: get routing preferences: %w", err)
		}
		prefs = domain.DefaultRoutingPreferences(m.ID)
	}
	applySettingsToRouting(m, prefs)
	return prefs, nil
}

// applySettingsToRouting 把存於 merchants.settings 的路由欄位合併進偏好。
func applySettingsToRouting(m *domain.Merchant, p *domain.RoutingPreferences) {
	p.FailoverEnabled = m.Settings.EffectiveAllowFailover()
	p.MaxAttempts = m.Settings.EffectiveMaxAttempts()
	p.FallbackProviders = append([]string{}, m.Settings.FallbackProviders...)
	if p.Rules == nil {
		p.Rules = []domain.RoutingRule{}
	}
}

// UpdateRoutingPreferencesInput 對應 UpdateRoutingPreferencesRequest（整份覆寫）。
type UpdateRoutingPreferencesInput struct {
	MerchantID        string
	Rules             []domain.RoutingRule
	FallbackProviders []string
	FailoverEnabled   bool
	MaxAttempts       int
}

// UpdateRoutingPreferences 整份覆寫（非 merge）；rules 寫 routing_preferences，其餘寫 merchants.settings，同交易 + outbox。
func (s *Service) UpdateRoutingPreferences(ctx context.Context, in UpdateRoutingPreferencesInput) (*domain.RoutingPreferences, error) {
	var out *domain.RoutingPreferences
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		m, err := s.lockMerchant(ctx, in.MerchantID)
		if err != nil {
			return err
		}
		if err := m.AssertWritable(); err != nil {
			return err
		}
		prefs := &domain.RoutingPreferences{
			MerchantID:        m.ID,
			Rules:             append([]domain.RoutingRule{}, in.Rules...),
			FallbackProviders: append([]string{}, in.FallbackProviders...),
			FailoverEnabled:   in.FailoverEnabled,
			MaxAttempts:       in.MaxAttempts,
		}
		if err := prefs.Validate(s.providers); err != nil {
			return err
		}
		if err := s.routing.Upsert(ctx, prefs); err != nil {
			return fmt.Errorf("app: upsert routing preferences: %w", err)
		}
		failover := prefs.FailoverEnabled
		m.Settings.AllowFailover = &failover
		m.Settings.MaxAttempts = prefs.MaxAttempts
		m.Settings.FallbackProviders = prefs.FallbackProviders
		m.UpdatedAt = s.clock.Now()
		if err := s.merchants.Update(ctx, m); err != nil {
			return fmt.Errorf("app: update merchant settings: %w", err)
		}
		out = prefs
		return s.emit(ctx, AggregateRoutingPreferences, EventRoutingPreferencesUpdated, m.PublicID(), routingEventData(prefs))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

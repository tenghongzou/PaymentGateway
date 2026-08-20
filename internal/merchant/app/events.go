package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// 事件型別（topic merchant.events；消費者：api-gateway 快取失效、webhook-service 端點讀模型、payment-service 路由快取）。
const (
	EventMerchantCreated           = "merchant.created"
	EventMerchantUpdated           = "merchant.updated"
	EventAPIKeyCreated             = "merchant.api_key.created"
	EventAPIKeyRevoked             = "merchant.api_key.revoked"
	EventWebhookEndpointCreated    = "merchant.webhook_endpoint.created"
	EventWebhookEndpointUpdated    = "merchant.webhook_endpoint.updated"
	EventWebhookEndpointDeleted    = "merchant.webhook_endpoint.deleted"
	EventWebhookSecretRotated      = "merchant.webhook_endpoint.secret_rotated"
	EventRoutingPreferencesUpdated = "merchant.routing_preferences.updated"
)

// 聚合型別（outbox.aggregate_type）。
const (
	AggregateMerchant           = "merchant"
	AggregateAPIKey             = "api_key"
	AggregateWebhookEndpoint    = "webhook_endpoint"
	AggregateRoutingPreferences = "routing_preferences"
)

// 事件 header。
const (
	headerContentType   = "content_type"
	headerSchemaVersion = "schema_version"
	contentTypeJSON     = "application/json"
	schemaVersion       = "1"
)

// EventEnvelope 為 merchant.events 的 JSON payload 外層（Phase 0 用 JSON；TODO：改 pg.merchant.v1 protobuf 事件）。
type EventEnvelope struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	MerchantID string    `json:"merchant_id"`
	Data       any       `json:"data"`
}

// MerchantEventData 為 merchant.* 事件的 data。
type MerchantEventData struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Status          string            `json:"status"`
	Country         string            `json:"country"`
	DefaultCurrency string            `json:"default_currency"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Version         int               `json:"version"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// APIKeyEventData 為 api_key.* 事件的 data（不含 hash / secret）。
type APIKeyEventData struct {
	ID         string     `json:"id"`
	MerchantID string     `json:"merchant_id"`
	Mode       string     `json:"mode"`
	Prefix     string     `json:"prefix"`
	Status     string     `json:"status"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

// WebhookEndpointEventData 為 webhook_endpoint.* 事件的 data（不含 secret；webhook-service 以 ListWebhookEndpoints(include_secrets) 取得）。
type WebhookEndpointEventData struct {
	ID              string            `json:"id"`
	MerchantID      string            `json:"merchant_id"`
	URL             string            `json:"url"`
	Description     string            `json:"description,omitempty"`
	EnabledEvents   []string          `json:"enabled_events"`
	Status          string            `json:"status"`
	Mode            string            `json:"mode"`
	SecretRotatedAt *time.Time        `json:"secret_rotated_at,omitempty"`
	DeletedAt       *time.Time        `json:"deleted_at,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Version         int               `json:"version"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// RoutingPreferencesEventData 為 routing_preferences.updated 的 data。
type RoutingPreferencesEventData struct {
	MerchantID        string               `json:"merchant_id"`
	Rules             []domain.RoutingRule `json:"rules"`
	FallbackProviders []string             `json:"fallback_providers"`
	FailoverEnabled   bool                 `json:"failover_enabled"`
	MaxAttempts       int                  `json:"max_attempts"`
	Version           int                  `json:"version"`
}

func merchantEventData(m *domain.Merchant) MerchantEventData {
	return MerchantEventData{
		ID: m.PublicID(), Name: m.Name, Status: string(m.Status), Country: m.Country, DefaultCurrency: m.DefaultCurrency,
		Metadata: m.Metadata, Version: m.Version, UpdatedAt: m.UpdatedAt,
	}
}

func apiKeyEventData(k *domain.ApiKey, now time.Time, reason string) APIKeyEventData {
	return APIKeyEventData{
		ID: k.PublicID(), MerchantID: ids.Format(ids.PrefixMerchant, k.MerchantID), Mode: string(k.Mode), Prefix: k.Prefix,
		Status: string(k.Status(now)), Scopes: k.Scopes, ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt, Reason: reason,
	}
}

func webhookEndpointEventData(e *domain.WebhookEndpoint) WebhookEndpointEventData {
	status := string(e.Status)
	switch {
	case e.IsDeleted():
		status = "deleted"
	case e.AutoDisabled:
		status = "auto_disabled"
	}
	return WebhookEndpointEventData{
		ID: e.PublicID(), MerchantID: ids.Format(ids.PrefixMerchant, e.MerchantID), URL: e.URL, Description: e.Description,
		EnabledEvents: e.EnabledEvents, Status: status, Mode: string(e.Mode), SecretRotatedAt: e.SecretRotatedAt,
		DeletedAt: e.DeletedAt, Metadata: e.Metadata, Version: e.Version, UpdatedAt: e.UpdatedAt,
	}
}

func routingEventData(p *domain.RoutingPreferences) RoutingPreferencesEventData {
	return RoutingPreferencesEventData{
		MerchantID: ids.Format(ids.PrefixMerchant, p.MerchantID), Rules: p.Rules, FallbackProviders: p.FallbackProviders,
		FailoverEnabled: p.FailoverEnabled, MaxAttempts: p.MaxAttempts, Version: p.Version,
	}
}

// emit 在目前交易內寫入一筆 outbox 事件；aggregate_id 一律用商戶 public id（同商戶事件保序）。
func (s *Service) emit(ctx context.Context, aggregateType, eventType, merchantPublicID string, data any) error {
	env := EventEnvelope{
		ID:         ids.NewUUID().String(),
		Type:       eventType,
		OccurredAt: s.clock.Now(),
		MerchantID: merchantPublicID,
		Data:       data,
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("app: marshal event %s: %w", eventType, err)
	}
	_, err = s.outbox.Insert(ctx, OutboxMessage{
		ID:            env.ID,
		AggregateType: aggregateType,
		AggregateID:   merchantPublicID,
		EventType:     eventType,
		Payload:       payload,
		Headers:       map[string]string{headerContentType: contentTypeJSON, headerSchemaVersion: schemaVersion},
	})
	if err != nil {
		return fmt.Errorf("app: outbox %s: %w", eventType, err)
	}
	return nil
}

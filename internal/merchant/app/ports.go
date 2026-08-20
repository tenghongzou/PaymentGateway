// Package app 為 merchant-service 的應用層：ports（repo / outbox / clock 介面）與對應 14 個 rpc 的 use cases。
//
// 依賴反轉：只 import domain 與 pkg/，不 import adapter；所有寫入 use case 以 TxManager 包交易並同交易寫 outbox。
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// Clock 提供現在時間（測試可注入固定時鐘）。
type Clock interface {
	Now() time.Time
}

// SystemClock 為真實時鐘（UTC）。
type SystemClock struct{}

// Now 回傳 time.Now().UTC()。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// TxManager 執行交易：fn 內透過 ctx 取得同一交易；fn 回錯誤則 rollback。
// 巢狀呼叫（ctx 已帶交易）時直接沿用外層交易。
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// 分頁預設值（docs/03 §3.1）。
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Page 為分頁輸入（cursor-based）。
type Page struct {
	Size  int
	Token string
}

// Normalize 套用預設與上限。
func (p Page) Normalize() Page {
	if p.Size <= 0 {
		p.Size = DefaultPageSize
	}
	if p.Size > MaxPageSize {
		p.Size = MaxPageSize
	}
	return p
}

// MerchantFilter 為 ListMerchants 篩選條件。
type MerchantFilter struct {
	Status  domain.Status
	Country string
}

// MerchantRepo 為商戶 repository。找不到時回傳滿足 errors.Is(err, domain.ErrNotFound) 的錯誤。
type MerchantRepo interface {
	Create(ctx context.Context, m *domain.Merchant) error
	Get(ctx context.Context, id uuid.UUID) (*domain.Merchant, error)
	// GetForUpdate 在交易內以 row lock 取得商戶，用來序列化同商戶的寫入（數量上限檢查等）。
	GetForUpdate(ctx context.Context, id uuid.UUID) (*domain.Merchant, error)
	FindByExternalRef(ctx context.Context, ref string) (*domain.Merchant, error)
	// Update 以 version 做樂觀鎖；成功後 m.Version / m.UpdatedAt 更新；衝突回 domain.ErrConcurrentModify。
	Update(ctx context.Context, m *domain.Merchant) error
	// List 依 created_at DESC, id DESC 回傳一頁與下一頁 token（無下一頁為空）。
	List(ctx context.Context, f MerchantFilter, p Page) ([]*domain.Merchant, string, error)
}

// ApiKeyFilter 為 ListApiKeys 篩選條件。
type ApiKeyFilter struct { //nolint:revive // 與 proto 命名一致
	Mode            domain.Mode
	IncludeInactive bool
}

// ApiKeyRepo 為 API Key repository。
type ApiKeyRepo interface { //nolint:revive // 與 proto 命名一致
	Create(ctx context.Context, k *domain.ApiKey) error
	Get(ctx context.Context, merchantID, id uuid.UUID) (*domain.ApiKey, error)
	// FindByPrefix 以 prefix（pk_live_ + lookup_id）查候選 key，最多 2 把（prefix 有 UNIQUE 約束，實際為 0 或 1）。
	FindByPrefix(ctx context.Context, prefix string) ([]*domain.ApiKey, error)
	Update(ctx context.Context, k *domain.ApiKey) error
	// CountActive 計算商戶某 mode 未撤銷且未過期的 key 數。
	CountActive(ctx context.Context, merchantID uuid.UUID, mode domain.Mode, now time.Time) (int, error)
	List(ctx context.Context, merchantID uuid.UUID, f ApiKeyFilter, p Page) ([]*domain.ApiKey, string, error)
	// TouchLastUsed 更新 last_used_at（低頻、非同步）。
	TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error
}

// WebhookEndpointFilter 為 ListWebhookEndpoints 篩選條件。
type WebhookEndpointFilter struct {
	Mode           domain.Mode
	IncludeDeleted bool
}

// WebhookEndpointRepo 為 Webhook 端點 repository。
type WebhookEndpointRepo interface {
	Create(ctx context.Context, e *domain.WebhookEndpoint) error
	Get(ctx context.Context, merchantID, id uuid.UUID) (*domain.WebhookEndpoint, error)
	Update(ctx context.Context, e *domain.WebhookEndpoint) error
	// CountLive 計算商戶未刪除的端點數。
	CountLive(ctx context.Context, merchantID uuid.UUID) (int, error)
	List(ctx context.Context, merchantID uuid.UUID, f WebhookEndpointFilter, p Page) ([]*domain.WebhookEndpoint, string, error)
}

// RoutingPrefRepo 為 routing_preferences 表的 repository（只負責 rules / version / updated_at；
// failover / max_attempts / fallback_providers 存於 merchants.settings，由 MerchantRepo 處理）。
type RoutingPrefRepo interface {
	// Get 找不到時回 domain.ErrNotFound（表示商戶從未設定）。
	Get(ctx context.Context, merchantID uuid.UUID) (*domain.RoutingPreferences, error)
	// Upsert 整份覆寫 rules 並遞增 version；成功後回填 p.Version / p.UpdatedAt。
	Upsert(ctx context.Context, p *domain.RoutingPreferences) error
}

// OutboxMessage 為要寫入 outbox 的事件（與 pkg/outbox.Message 對應，避免 app 直接依賴 pgx）。
type OutboxMessage struct {
	ID            string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	Headers       map[string]string
}

// OutboxStore 在目前交易內寫入 outbox；回傳 event id。
type OutboxStore interface {
	Insert(ctx context.Context, msg OutboxMessage) (string, error)
}

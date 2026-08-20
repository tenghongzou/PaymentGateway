package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// 預設上限（proto 註解）。
const (
	DefaultMaxAPIKeysPerMode   = 10
	DefaultMaxWebhookEndpoints = 16
	DefaultLastUsedInterval    = time.Minute
)

// Config 為 Service 的行為設定。
type Config struct {
	// AllowInsecureWebhookURL 為 true（dev）時允許 http / localhost / 私有 IP 的 webhook URL。
	AllowInsecureWebhookURL bool
	// URLResolver 非 nil 時，建立 / 更新端點會解析 DNS 並拒絕私有網段（生產建議 net.DefaultResolver）。
	URLResolver domain.Resolver
	// KnownProviders 為已註冊的 adapter 名稱（路由規則 provider 檢查）；空表示不檢查。
	KnownProviders []string
	// MaxAPIKeysPerMode / MaxWebhookEndpoints 為 0 時使用預設（10 / 16）。
	MaxAPIKeysPerMode   int
	MaxWebhookEndpoints int
	// LastUsedInterval 為同一把 key 更新 last_used_at 的最小間隔（0 = 1 分鐘）。
	LastUsedInterval time.Duration
	// SyncLastUsed 為 true 時同步更新 last_used_at（測試用）；預設非同步。
	SyncLastUsed bool
}

// Deps 為 Service 的依賴。
type Deps struct {
	Tx        TxManager
	Merchants MerchantRepo
	APIKeys   ApiKeyRepo
	Webhooks  WebhookEndpointRepo
	Routing   RoutingPrefRepo
	Outbox    OutboxStore
	Clock     Clock
	Cipher    domain.SecretCipher
	Logger    *slog.Logger
}

// Service 實作全部 use cases。
type Service struct {
	tx        TxManager
	merchants MerchantRepo
	keys      ApiKeyRepo
	hooks     WebhookEndpointRepo
	routing   RoutingPrefRepo
	outbox    OutboxStore
	clock     Clock
	cipher    domain.SecretCipher
	log       *slog.Logger
	cfg       Config
	providers domain.ProviderSet
	urlPolicy domain.URLPolicy
	// dummyHash 用於 key 不存在時仍執行一次 Argon2id，避免以時間差列舉 key（docs/06 §3.3）。
	dummyHash string
	// lastUsed 記錄每把 key 最近一次寫入 last_used_at 的時間（節流）。
	lastUsed sync.Map
	touchWG  sync.WaitGroup
}

// New 建立 Service。
func New(d Deps, cfg Config) (*Service, error) {
	if d.Tx == nil || d.Merchants == nil || d.APIKeys == nil || d.Webhooks == nil || d.Routing == nil || d.Outbox == nil {
		return nil, errors.New("app: all repositories, tx manager and outbox are required")
	}
	if d.Cipher == nil {
		return nil, errors.New("app: secret cipher is required")
	}
	if d.Clock == nil {
		d.Clock = SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if cfg.MaxAPIKeysPerMode <= 0 {
		cfg.MaxAPIKeysPerMode = DefaultMaxAPIKeysPerMode
	}
	if cfg.MaxWebhookEndpoints <= 0 {
		cfg.MaxWebhookEndpoints = DefaultMaxWebhookEndpoints
	}
	if cfg.LastUsedInterval <= 0 {
		cfg.LastUsedInterval = DefaultLastUsedInterval
	}
	return &Service{
		tx: d.Tx, merchants: d.Merchants, keys: d.APIKeys, hooks: d.Webhooks, routing: d.Routing, outbox: d.Outbox,
		clock: d.Clock, cipher: d.Cipher, log: d.Logger, cfg: cfg,
		providers: domain.NewProviderSet(cfg.KnownProviders),
		urlPolicy: domain.URLPolicy{AllowInsecure: cfg.AllowInsecureWebhookURL, Resolver: cfg.URLResolver},
		dummyHash: domain.HashArgon2id("pk_test_" + strings.Repeat("0", 43)),
	}, nil
}

// Close 等待背景的 last_used_at 更新結束（關機時呼叫）。
func (s *Service) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.touchWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// loadMerchant 解析 public id 並取得商戶。
func (s *Service) loadMerchant(ctx context.Context, publicID string) (*domain.Merchant, error) {
	id, err := domain.ParseMerchantID(publicID)
	if err != nil {
		return nil, err
	}
	return s.merchants.Get(ctx, id)
}

// lockMerchant 解析 public id 並以 row lock 取得商戶（交易內）。
func (s *Service) lockMerchant(ctx context.Context, publicID string) (*domain.Merchant, error) {
	id, err := domain.ParseMerchantID(publicID)
	if err != nil {
		return nil, err
	}
	return s.merchants.GetForUpdate(ctx, id)
}

// keyAAD / webhookAAD 為欄位級加密的 AAD（docs/06 §7.3）。
func keyAAD(id uuid.UUID, column string) string {
	return domain.SecretAAD("api_keys", column, id.String())
}

func webhookAAD(id uuid.UUID, column string) string {
	return domain.SecretAAD("webhook_endpoints", column, id.String())
}

package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
)

// Config 為 use case 參數。
type Config struct {
	// MaxAttempts 為每筆付款最多的 authorize Attempt 數（PG_PROVIDER_FAILOVER_MAX_ATTEMPTS，上限 3）。
	MaxAttempts int
	// ProviderTimeout 為單次 adapter 呼叫逾時（PG_PROVIDER_TIMEOUT）。
	ProviderTimeout time.Duration
	// SagaBudget 為整個 authorize saga 的時間上限（docs/02 §4.2：25s）。
	SagaBudget time.Duration
	// MinRemainingForFailover 為再開一個 Attempt 所需的最少剩餘預算（docs/05 §3.3：3s）。
	MinRemainingForFailover time.Duration
	// ResolveDelays 為 timeout 後 GetPaymentStatus 收斂的間隔（預設 1s/2s/4s；測試可縮短）。
	ResolveDelays []time.Duration
	// RetrySameProviderOnUnavailable：沒有其他候選時，provider_unavailable / rate_limited 允許在同一 Provider 以新 Attempt 重試一次
	// （Phase 0 僅有 provider-mock 時讓 tok_unavailable_once 可收斂；docs/02 §11 原則上 unavailable 直接切換）。
	RetrySameProviderOnUnavailable bool
}

func (c Config) withDefaults() Config {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 2
	}
	if c.MaxAttempts > 3 {
		c.MaxAttempts = 3
	}
	if c.ProviderTimeout <= 0 {
		c.ProviderTimeout = 10 * time.Second
	}
	if c.SagaBudget <= 0 {
		c.SagaBudget = 25 * time.Second
	}
	if c.MinRemainingForFailover <= 0 {
		c.MinRemainingForFailover = 3 * time.Second
	}
	if len(c.ResolveDelays) == 0 {
		c.ResolveDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	}
	return c
}

// Deps 為 Service 的依賴。
type Deps struct {
	Repo      PaymentRepo
	Tx        TxManager
	Outbox    OutboxStore
	Providers ProviderRegistry
	Router    Router
	Clock     Clock
	Logger    *slog.Logger
	Config    Config
}

// Service 實作所有 payment use cases。
type Service struct {
	repo      PaymentRepo
	tx        TxManager
	outbox    OutboxStore
	providers ProviderRegistry
	router    Router
	clock     Clock
	log       *slog.Logger
	cfg       Config
}

// NewService 建立 Service。
func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{repo: d.Repo, tx: d.Tx, outbox: d.Outbox, providers: d.Providers, router: d.Router, clock: d.Clock, log: d.Logger, cfg: d.Config.withDefaults()}
}

// persist 在既有交易內寫入 payment 更新、attempt、事件與 outbox（每次狀態轉移的統一寫入路徑，docs/05 §0.3）。
func (s *Service) persist(ctx context.Context, p *domain.Payment, expectedVersion int, attempts []*domain.Attempt, events []domain.Event) error {
	if err := s.repo.UpdatePayment(ctx, p, expectedVersion); err != nil {
		return err
	}
	for _, a := range attempts {
		if err := s.repo.UpdateAttempt(ctx, a); err != nil {
			return err
		}
	}
	return s.appendEvents(ctx, p, events)
}

// appendEvents 寫 payment_events 與 outbox。
func (s *Service) appendEvents(ctx context.Context, p *domain.Payment, events []domain.Event) error {
	if len(events) == 0 {
		return nil
	}
	traceID := logx.TraceIDFromContext(ctx)
	if err := s.repo.AppendEvents(ctx, p, events, traceID); err != nil {
		return err
	}
	for _, ev := range events {
		msg, err := buildOutboxMessage(p, ev, traceID)
		if err != nil {
			return fmt.Errorf("build outbox message: %w", err)
		}
		if err := s.outbox.Insert(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

// providerFor 取得 Provider client；不存在時回傳 false。
func (s *Service) providerFor(name string) (ProviderClient, bool) {
	if s.providers == nil {
		return nil, false
	}
	return s.providers.Get(name)
}

// callCtx 為單次 adapter 呼叫建立 deadline（取 ProviderTimeout 與剩餘預算較小者）。
func (s *Service) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.cfg.ProviderTimeout)
}

var _ = outbox.Message{}

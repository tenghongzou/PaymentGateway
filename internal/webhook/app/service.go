package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
)

// ConsumerName 為 processed_events.consumer。
const ConsumerName = "webhook-service"

// EndpointLookupRetryIn 為端點資料暫時取不到時把 delivery 放回佇列的等待時間。
const EndpointLookupRetryIn = 30 * time.Second

// Deps 為 Service 的依賴。
type Deps struct {
	Tx         Transactor
	Inbox      Inbox
	Events     EventRepo
	Deliveries DeliveryRepo
	Endpoints  EndpointSource
	// Disabler 可為 nil（只記 log）。
	Disabler EndpointDisabler
	Sender   HTTPSender
	// Clock 預設 time.Now。
	Clock Clock
	// Policy 預設 StrictPolicy。
	Policy domain.URLPolicy
	// Rand 產生 [0,1) 亂數供 jitter；預設 math/rand/v2。
	Rand func() float64
	// Consumer 預設 ConsumerName。
	Consumer string
	Logger   *slog.Logger
}

// Service 實作 webhook-service 的 use cases。
type Service struct {
	tx         Transactor
	inbox      Inbox
	events     EventRepo
	deliveries DeliveryRepo
	endpoints  EndpointSource
	disabler   EndpointDisabler
	sender     HTTPSender
	clock      Clock
	policy     domain.URLPolicy
	rnd        func() float64
	signer     domain.Signer
	consumer   string
	log        *slog.Logger

	// retryKeys 為 RetryDelivery 的冪等鍵（行程內、24h）。TODO: 多副本時改存 Redis / DB。
	retryMu   sync.Mutex
	retryKeys map[string]retryRecord
}

type retryRecord struct {
	deliveryID uuid.UUID
	at         time.Time
}

// New 建立 Service。
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = ClockFunc(time.Now)
	}
	if d.Rand == nil {
		d.Rand = rand.Float64
	}
	if d.Consumer == "" {
		d.Consumer = ConsumerName
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{
		tx: d.Tx, inbox: d.Inbox, events: d.Events, deliveries: d.Deliveries, endpoints: d.Endpoints,
		disabler: d.Disabler, sender: d.Sender, clock: d.Clock, policy: d.Policy, rnd: d.Rand,
		consumer: d.Consumer, log: d.Logger, retryKeys: map[string]retryRecord{},
	}
}

// ---------------------------------------------------------------------------
// Ingest
// ---------------------------------------------------------------------------

// IngestResult 為 IngestEvent 的結果。
type IngestResult struct {
	// Duplicate 表示事件已處理過（processed_events 命中），本次未做任何寫入。
	Duplicate bool
	// Deliveries 為本次建立的 pending delivery 數。
	Deliveries int
}

// IngestPaymentEvent 把 Kafka 的 PaymentEvent 正規化後 Ingest。
func (s *Service) IngestPaymentEvent(ctx context.Context, pe *paymentv1.PaymentEvent) (IngestResult, error) {
	ev, err := domain.FromPaymentEvent(pe, s.clock.Now())
	if err != nil {
		return IngestResult{}, err
	}
	return s.IngestEvent(ctx, ev)
}

// IngestEvent 去重 → 寫入 webhook_events → 為每個訂閱的啟用端點建立 pending delivery（同一交易）。
// 端點查詢在交易外進行；失敗時回傳錯誤讓 consumer 重試（不 commit offset）。
func (s *Service) IngestEvent(ctx context.Context, ev *domain.Event) (IngestResult, error) {
	if ev == nil {
		return IngestResult{}, errors.New("webhook: nil event")
	}
	eps, err := s.endpoints.ListEndpoints(ctx, ev.MerchantID)
	if err != nil {
		return IngestResult{}, fmt.Errorf("webhook: list endpoints for %s: %w", domain.MerchantPublicID(ev.MerchantID), err)
	}
	ds := domain.FanOut(ev, eps, s.clock.Now)

	var res IngestResult
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		already, err := s.inbox.MarkProcessed(ctx, ev.ID, s.consumer)
		if err != nil {
			return err
		}
		if already {
			res.Duplicate = true
			return nil
		}
		if err := s.events.Insert(ctx, ev); err != nil {
			return err
		}
		if len(ds) > 0 {
			if err := s.deliveries.InsertPending(ctx, ds); err != nil {
				return err
			}
		}
		res.Deliveries = len(ds)
		return nil
	})
	if err != nil {
		return IngestResult{}, err
	}
	log := logx.FromContext(ctx).With("event_id", ev.PublicID(), "event_type", ev.Type, "merchant_id", domain.MerchantPublicID(ev.MerchantID))
	if res.Duplicate {
		log.Debug("webhook event already processed")
	} else {
		log.Info("webhook event ingested", "deliveries", res.Deliveries, "endpoints", len(eps))
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

// DispatchDue 取一批到期的 delivery 並以至多 concurrency 個 goroutine 投遞；回傳取件數。
func (s *Service) DispatchDue(ctx context.Context, batch, concurrency int) (int, error) {
	if batch <= 0 {
		batch = 50
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	claimed, err := s.deliveries.ClaimDue(ctx, s.clock.Now(), batch)
	if err != nil {
		return 0, fmt.Errorf("webhook: claim due deliveries: %w", err)
	}
	if len(claimed) == 0 {
		return 0, nil
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, d := range claimed {
		wg.Add(1)
		sem <- struct{}{}
		go func(d *domain.Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			s.Deliver(ctx, d)
		}(d)
	}
	wg.Wait()
	return len(claimed), nil
}

// Deliver 對一筆已取件（in_flight）的 delivery 執行一次 HTTP 投遞並持久化結果。
// 所有錯誤都已記錄並反映在 delivery 狀態，不回傳。
func (s *Service) Deliver(ctx context.Context, d *domain.Delivery) {
	log := s.log.With("delivery_id", d.PublicID(), "event_id", domain.EventPublicID(d.EventID),
		"event_type", d.EventType, "endpoint_id", domain.EndpointPublicID(d.EndpointID), "attempt", d.AttemptNo)
	now := s.clock.Now()

	ep, err := s.endpoints.GetEndpoint(ctx, d.MerchantID, d.EndpointID)
	if err != nil {
		// 端點資料暫時取不到：不算一次嘗試，稍後再試。
		log.Warn("endpoint lookup failed; releasing delivery", "err", err)
		if rerr := d.Release(now, EndpointLookupRetryIn, "endpoint lookup failed: "+err.Error()); rerr == nil {
			s.save(ctx, log, d, nil)
		}
		return
	}
	if ep == nil || !ep.Enabled() {
		reason := "endpoint not found"
		if ep != nil {
			reason = "endpoint " + string(ep.Status)
		}
		log.Info("endpoint unavailable; canceling delivery", "reason", reason)
		d.Cancel(now, reason)
		s.save(ctx, log, d, nil)
		return
	}

	outcome := s.send(ctx, d, ep)
	tr, att, err := d.ApplyOutcome(s.clock.Now(), outcome, s.rnd())
	if err != nil {
		log.Error("apply outcome failed", "err", err)
		return
	}
	s.save(ctx, log, d, att)

	attrs := []any{"status_code", outcome.StatusCode, "duration_ms", outcome.Duration.Milliseconds(), "result", string(d.Status)}
	if outcome.Err != nil {
		attrs = append(attrs, "err", outcome.Err.Error())
	}
	switch tr {
	case domain.TransitionSucceeded:
		log.Info("webhook delivered", attrs...)
	case domain.TransitionRetry:
		log.Warn("webhook delivery failed; scheduled retry", append(attrs, "next_attempt_at", d.NextAttemptAt)...)
	case domain.TransitionDeadLetter:
		// TODO: 寫 outbox webhook.delivery.dead_lettered（告警 / 商戶通知信）。
		log.Error("webhook delivery dead-lettered", attrs...)
	case domain.TransitionGone:
		log.Warn("endpoint returned 410 Gone; disabling endpoint", attrs...)
		s.disableEndpoint(ctx, log, ep, "endpoint returned 410 Gone")
	}
}

// send 驗證 URL、簽章並送出；任何前置失敗都以 Outcome.Err 表達（計入一次嘗試）。
func (s *Service) send(ctx context.Context, d *domain.Delivery, ep *domain.Endpoint) domain.Outcome {
	if _, err := s.policy.ValidateURL(ep.URL); err != nil {
		return domain.Outcome{Err: err}
	}
	ts := s.clock.Now().Unix()
	signature, err := s.signer.Sign(ep.ActiveSecrets(), ts, d.EventPayload)
	if err != nil {
		return domain.Outcome{Err: err}
	}
	return s.sender.Send(ctx, SendRequest{
		URL:  ep.URL,
		Body: d.EventPayload,
		Headers: map[string]string{
			"Content-Type":          domain.ContentTypeJSON,
			"User-Agent":            domain.UserAgent,
			domain.HeaderSignature:  signature,
			domain.HeaderEventID:    domain.EventPublicID(d.EventID),
			domain.HeaderEventType:  d.EventType,
			domain.HeaderDeliveryID: d.PublicID(),
			domain.HeaderAttempt:    strconv.Itoa(d.AttemptNo),
		},
	})
}

func (s *Service) save(ctx context.Context, log *slog.Logger, d *domain.Delivery, att *domain.Attempt) {
	// 用不受取消影響的 ctx 寫回結果：HTTP 已送出，狀態一定要落地，否則 reaper 會造成重複投遞。
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.deliveries.Save(saveCtx, d, att); err != nil {
		log.Error("save delivery failed", "err", err, "status", d.Status)
	}
}

func (s *Service) disableEndpoint(ctx context.Context, log *slog.Logger, ep *domain.Endpoint, reason string) {
	now := s.clock.Now()
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if n, err := s.deliveries.CancelForEndpoint(bg, ep.ID, now, reason); err != nil {
		log.Error("cancel deliveries for endpoint failed", "err", err)
	} else if n > 0 {
		log.Info("canceled pending deliveries for endpoint", "count", n)
	}
	if s.disabler != nil {
		if err := s.disabler.DisableEndpoint(bg, ep.MerchantID, ep.ID, reason); err != nil {
			log.Error("disable endpoint via merchant-service failed", "err", err)
		}
	}
	if inv, ok := s.endpoints.(EndpointInvalidator); ok {
		inv.Invalidate(ep.MerchantID)
	}
}

// ---------------------------------------------------------------------------
// Reaper
// ---------------------------------------------------------------------------

// ReapStuckInFlight 把 in_flight 超過 timeout 的 delivery 轉回 failed（worker 崩潰回收）。
func (s *Service) ReapStuckInFlight(ctx context.Context, timeout time.Duration) (int64, error) {
	if timeout <= 0 {
		timeout = domain.InFlightTimeout
	}
	now := s.clock.Now()
	n, err := s.deliveries.ReapStuck(ctx, now.Add(-timeout), now)
	if err != nil {
		return 0, fmt.Errorf("webhook: reap stuck deliveries: %w", err)
	}
	if n > 0 {
		s.log.Warn("reclaimed stuck in_flight deliveries", "count", n, "timeout", timeout)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Queries / manual retry
// ---------------------------------------------------------------------------

// GetDelivery 取得 delivery 與所有嘗試。
func (s *Service) GetDelivery(ctx context.Context, merchantID, deliveryID uuid.UUID) (*domain.Delivery, []*domain.Attempt, error) {
	d, err := s.deliveries.Get(ctx, merchantID, deliveryID)
	if err != nil {
		return nil, nil, err
	}
	atts, err := s.deliveries.ListAttempts(ctx, d.ID)
	if err != nil {
		return nil, nil, err
	}
	return d, atts, nil
}

// ListDeliveries 依條件分頁列出。
func (s *Service) ListDeliveries(ctx context.Context, f DeliveryFilter) (*DeliveryPage, error) {
	if f.PageSize <= 0 {
		f.PageSize = 20
	}
	if f.PageSize > 100 {
		f.PageSize = 100
	}
	return s.deliveries.List(ctx, f)
}

// ListEventTypes 回傳支援的事件類型。
func (s *Service) ListEventTypes() []domain.EventTypeInfo { return domain.EventTypes }

// LookupEndpoint 取得端點（查詢回應補 endpoint_url 用）；找不到回 nil。
func (s *Service) LookupEndpoint(ctx context.Context, merchantID, endpointID uuid.UUID) *domain.Endpoint {
	ep, err := s.endpoints.GetEndpoint(ctx, merchantID, endpointID)
	if err != nil {
		return nil
	}
	return ep
}

// RetryDelivery 手動重送：重置嘗試視窗並排入立即投遞。
// 前置：status ∈ {failed, dead_letter, succeeded}、端點啟用中、idempotencyKey 非空；
// 同一 (merchant, key) 重複呼叫回傳同一筆 delivery 的現況而不再重置。
func (s *Service) RetryDelivery(ctx context.Context, merchantID, deliveryID uuid.UUID, idempotencyKey string) (*domain.Delivery, error) {
	if idempotencyKey == "" {
		return nil, domain.ErrIdempotencyKeyMissing
	}
	idemKey := merchantID.String() + ":" + idempotencyKey
	if prev, ok := s.retryKeyGet(idemKey); ok {
		if prev != deliveryID {
			return nil, domain.ErrIdempotencyKeyMissing.WithMessage("Idempotency-Key was already used for another delivery.")
		}
		return s.deliveries.Get(ctx, merchantID, deliveryID)
	}
	d, err := s.deliveries.Get(ctx, merchantID, deliveryID)
	if err != nil {
		return nil, err
	}
	if !d.CanRetryManually() {
		return nil, domain.ErrDeliveryNotRetryable
	}
	ep, err := s.endpoints.GetEndpoint(ctx, merchantID, d.EndpointID)
	if err != nil {
		return nil, fmt.Errorf("webhook: lookup endpoint: %w", err)
	}
	if ep == nil || !ep.Enabled() {
		return nil, domain.ErrEndpointUnavailable
	}
	if err := d.ResetForRetry(s.clock.Now()); err != nil {
		return nil, err
	}
	if err := s.deliveries.Save(ctx, d, nil); err != nil {
		return nil, err
	}
	s.retryKeyPut(idemKey, deliveryID)
	s.log.Info("webhook delivery manually retried", "delivery_id", d.PublicID(), "merchant_id", domain.MerchantPublicID(merchantID))
	return d, nil
}

const retryKeyTTL = 24 * time.Hour

func (s *Service) retryKeyGet(key string) (uuid.UUID, bool) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	rec, ok := s.retryKeys[key]
	if !ok {
		return uuid.Nil, false
	}
	if s.clock.Now().Sub(rec.at) > retryKeyTTL {
		delete(s.retryKeys, key)
		return uuid.Nil, false
	}
	return rec.deliveryID, true
}

func (s *Service) retryKeyPut(key string, id uuid.UUID) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	now := s.clock.Now()
	// 順手清理過期鍵，避免無界成長。
	if len(s.retryKeys) > 10000 {
		for k, rec := range s.retryKeys {
			if now.Sub(rec.at) > retryKeyTTL {
				delete(s.retryKeys, k)
			}
		}
	}
	s.retryKeys[key] = retryRecord{deliveryID: id, at: now}
}

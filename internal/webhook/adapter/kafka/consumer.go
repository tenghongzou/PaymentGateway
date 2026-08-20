// Package kafka 為 webhook-service 的 Kafka adapter：消費 payment.events（consumer group webhook-service），
// 反序列化 pg.payment.v1.PaymentEvent 後交給 app.Service.IngestPaymentEvent（processed_events 去重）。
package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/protobuf/proto"

	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
)

// DefaultGroup 為 consumer group 名稱。
const DefaultGroup = "webhook-service"

// Ingester 為 consumer 需要的 use case（app.Service 滿足）。
type Ingester interface {
	IngestPaymentEvent(ctx context.Context, pe *paymentv1.PaymentEvent) (app.IngestResult, error)
}

// Consumer 包裝 eventbus.Consumer。
type Consumer struct {
	c   *eventbus.Consumer
	h   *Handler
	log *slog.Logger
}

// Config 為 consumer 設定。
type Config struct {
	eventbus.Options
	// Group 預設 DefaultGroup。
	Group string
	// Topics 預設 [payment.events]。
	Topics []string
	// DLQ 非 nil 時 handler 重試耗盡的訊息送 <topic>.dlq；nil 時 poison message 直接略過（log error）。
	DLQ *eventbus.Producer
}

// NewConsumer 建立 consumer。
func NewConsumer(cfg Config, svc Ingester, log *slog.Logger) (*Consumer, error) {
	if cfg.Group == "" {
		cfg.Group = DefaultGroup
	}
	if len(cfg.Topics) == 0 {
		cfg.Topics = []string{eventbus.TopicPaymentEvents}
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Options.Logger == nil {
		cfg.Options.Logger = log
	}
	c, err := eventbus.NewConsumer(eventbus.ConsumerConfig{Options: cfg.Options, Group: cfg.Group, Topics: cfg.Topics, DLQ: cfg.DLQ})
	if err != nil {
		return nil, err
	}
	return &Consumer{c: c, h: NewHandler(svc, log, cfg.DLQ == nil), log: log}, nil
}

// Run 持續消費直到 ctx 結束。
func (c *Consumer) Run(ctx context.Context) error { return c.c.Run(ctx, c.h.Handle) }

// Handler 為單筆訊息處理邏輯（與 Kafka client 解耦，便於測試）。
type Handler struct {
	svc        Ingester
	log        *slog.Logger
	skipPoison bool
}

// NewHandler 建立 Handler；skipPoison=true 時無法反序列化的訊息會被略過並 commit（沒有 DLQ 時避免卡住分區）。
func NewHandler(svc Ingester, log *slog.Logger, skipPoison bool) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{svc: svc, log: log, skipPoison: skipPoison}
}

// Handle 實作 eventbus.Handler：回 nil 才 commit offset。
func (h *Handler) Handle(ctx context.Context, rec eventbus.Record) error {
	log := h.log.With("topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset, "event_id", rec.EventID())
	ctx = logx.IntoContext(ctx, log)

	var pe paymentv1.PaymentEvent
	if err := proto.Unmarshal(rec.Value, &pe); err != nil {
		return h.poison(log, fmt.Errorf("kafka: unmarshal PaymentEvent: %w", err))
	}
	if pe.GetEventId() == "" && rec.EventID() != "" {
		pe.EventId = rec.EventID()
	}
	res, err := h.svc.IngestPaymentEvent(ctx, &pe)
	switch {
	case err == nil:
		if res.Duplicate {
			log.Debug("payment event already processed")
		}
		return nil
	case errors.Is(err, domain.ErrUnsupportedEvent), errors.Is(err, domain.ErrInvalidID):
		// 未知事件型別（前向相容）或 ID 格式異常：不可能靠重試修復。
		return h.poison(log, err)
	default:
		// DB / merchant-service 暫時失敗：回錯誤讓 eventbus 重試（不 commit）。
		return err
	}
}

func (h *Handler) poison(log *slog.Logger, err error) error {
	if h.skipPoison {
		log.Error("skipping poison message", "err", err)
		return nil
	}
	return err
}

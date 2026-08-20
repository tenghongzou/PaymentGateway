// Package kafka 為 reconciliation-service 的 Kafka adapter：消費 payment.events 維護讀模型。
package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// DefaultGroup 為 consumer group（deploy/helm values：PG_KAFKA_CONSUMER_GROUP=reconciliation-service）。
const DefaultGroup = "reconciliation-service"

// EventHandler 為 app 層的事件處理入口（由 *app.Service.HandlePaymentEvent 實作）。
type EventHandler interface {
	HandlePaymentEvent(ctx context.Context, eventID string, payload []byte) error
}

// Config 為 consumer 設定。
type Config struct {
	eventbus.Options
	// Group 空 → DefaultGroup。
	Group string
	// Topics 空 → payment.events。
	Topics []string
	// DLQ 非 nil 時，重試耗盡（含 poison）的訊息送到 <topic>.dlq。
	DLQ *eventbus.Producer
}

// Consumer 包裝 pkg/eventbus.Consumer。
type Consumer struct {
	inner   *eventbus.Consumer
	handler EventHandler
	log     *slog.Logger
}

// NewConsumer 建立 consumer。
func NewConsumer(cfg Config, handler EventHandler) (*Consumer, error) {
	if cfg.Group == "" {
		cfg.Group = DefaultGroup
	}
	if len(cfg.Topics) == 0 {
		cfg.Topics = []string{eventbus.TopicPaymentEvents}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	inner, err := eventbus.NewConsumer(eventbus.ConsumerConfig{
		Options: cfg.Options, Group: cfg.Group, Topics: cfg.Topics, DLQ: cfg.DLQ,
	})
	if err != nil {
		return nil, fmt.Errorf("kafka: new consumer: %w", err)
	}
	return &Consumer{inner: inner, handler: handler, log: cfg.Logger.With("component", "payment-events-consumer")}, nil
}

// Run 持續消費直到 ctx 結束（作為 pkg/app.Worker）。
func (c *Consumer) Run(ctx context.Context) error {
	return c.inner.Run(ctx, c.Handle)
}

// Handle 處理單筆訊息（公開以便測試）。
func (c *Consumer) Handle(ctx context.Context, rec eventbus.Record) error {
	eventID := EventUUID(rec.EventID(), "")
	if eventID == "" {
		// header 缺 event_id：嘗試從 payload 取（需要反序列化，交給 app 層的 poison 判斷前先盡力解析）。
		eventID = EventUUID("", payloadEventID(rec.Value))
	}
	err := c.handler.HandlePaymentEvent(ctx, eventID, rec.Value)
	var poison *app.ErrPoisonMessage
	if errors.As(err, &poison) {
		c.log.ErrorContext(ctx, "poison payment event", "topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset, "err", err)
	}
	return err
}

// EventUUID 把 header 的 event_id（uuid）或 payload 的 event_id（evt_ + ULID，內容同為 uuid）正規化成 uuid 字串；
// 兩者都無法解析時回空字串。processed_events.event_id 為 uuid 欄位。
func EventUUID(headerID, payloadID string) string {
	if headerID != "" {
		if u, err := uuid.Parse(headerID); err == nil {
			return u.String()
		}
		if _, u, err := ids.Parse(headerID); err == nil {
			return u.String()
		}
		// 非 uuid 的自訂 event_id：以 v5 雜湊維持去重能力。
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte(headerID)).String()
	}
	if payloadID != "" {
		if _, u, err := ids.Parse(payloadID); err == nil {
			return u.String()
		}
		if u, err := uuid.Parse(payloadID); err == nil {
			return u.String()
		}
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte(payloadID)).String()
	}
	return ""
}

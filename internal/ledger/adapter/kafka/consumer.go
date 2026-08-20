// Package kafka 為 ledger-service 的 Kafka adapter：以 pkg/eventbus.Consumer 消費 payment.events，
// 交給 app.Service.HandlePaymentEvent（去重 + 記帳同一交易），handler 成功後才 commit offset。
package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tenghongzou/paymentgateway/internal/ledger/app"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
)

// DefaultGroup 為 consumer group 名稱。
const DefaultGroup = "ledger-service"

// Config 為 PaymentConsumer 設定。
type Config struct {
	Brokers      []string
	Group        string
	ClientID     string
	SASLUsername string
	SASLPassword string
	TLS          bool
	// DLQ 非 nil 時，重試耗盡的訊息送到 payment.events.dlq 並 commit（不阻塞分區）。
	DLQ    *eventbus.Producer
	Logger *slog.Logger
}

// PaymentConsumer 消費 payment.events。
type PaymentConsumer struct {
	consumer *eventbus.Consumer
	svc      *app.Service
	log      *slog.Logger
}

// NewPaymentConsumer 建立 consumer（不會連線，Run 時才開始 poll）。
func NewPaymentConsumer(cfg Config, svc *app.Service) (*PaymentConsumer, error) {
	if cfg.Group == "" {
		cfg.Group = DefaultGroup
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "ledger-service"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	c, err := eventbus.NewConsumer(eventbus.ConsumerConfig{
		Options: eventbus.Options{
			Brokers: cfg.Brokers, ClientID: cfg.ClientID,
			SASLUsername: cfg.SASLUsername, SASLPassword: cfg.SASLPassword, TLS: cfg.TLS, Logger: cfg.Logger,
		},
		Group:  cfg.Group,
		Topics: []string{eventbus.TopicPaymentEvents},
		DLQ:    cfg.DLQ,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger/kafka: %w", err)
	}
	return &PaymentConsumer{consumer: c, svc: svc, log: cfg.Logger.With("component", "ledger-payment-consumer")}, nil
}

// Run 持續消費直到 ctx 結束（供 app.Worker 使用）。
func (c *PaymentConsumer) Run(ctx context.Context) error {
	return c.consumer.Run(ctx, c.handle)
}

// handle 為 eventbus.Handler：記錄錯誤類別後交給 app。
func (c *PaymentConsumer) handle(ctx context.Context, rec eventbus.Record) error {
	err := c.svc.HandlePaymentEvent(ctx, rec)
	if err != nil {
		_, eventID := app.EventIDFromRecord(rec)
		attrs := []any{"topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset, "event_id", eventID, "err", err}
		if errors.Is(err, app.ErrPoisonMessage) {
			c.log.ErrorContext(ctx, "poison payment event", attrs...)
		} else {
			c.log.WarnContext(ctx, "payment event handling failed", attrs...)
		}
	}
	return err
}

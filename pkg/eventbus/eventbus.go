// Package eventbus 封裝 franz-go（Kafka）的 producer / consumer。
//
// Producer 實作 pkg/outbox.Publisher；Consumer 以 consumer group 手動 commit（handler 成功後才 commit）。
package eventbus

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// Topic 常數（docs/01 §4）。
const (
	TopicPaymentEvents        = "payment.events"
	TopicRefundEvents         = "refund.events"
	TopicLedgerEvents         = "ledger.events"
	TopicMerchantEvents       = "merchant.events"
	TopicReconciliationEvents = "reconciliation.events"
	// DLQSuffix 為死信 topic 後綴（<topic>.dlq）。
	DLQSuffix = ".dlq"
)

// Kafka record header 名稱。
const (
	HeaderEventID       = "event_id"
	HeaderEventType     = "event_type"
	HeaderAggregateType = "aggregate_type"
	HeaderTraceParent   = "traceparent"
	HeaderSchemaVersion = "schema_version"
	HeaderOriginalTopic = "original_topic"
	HeaderError         = "error"
)

// Options 為連線選項。
type Options struct {
	Brokers      []string
	ClientID     string
	SASLUsername string
	SASLPassword string
	TLS          bool
	Logger       *slog.Logger
}

func (o Options) clientOpts() ([]kgo.Opt, error) {
	if len(o.Brokers) == 0 {
		return nil, errors.New("eventbus: no brokers configured")
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(o.Brokers...),
		kgo.ClientID(o.ClientID),
	}
	if o.TLS {
		opts = append(opts, kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	}
	if o.SASLUsername != "" {
		opts = append(opts, kgo.SASL(scram.Auth{User: o.SASLUsername, Pass: o.SASLPassword}.AsSha512Mechanism()))
	}
	return opts, nil
}

// Producer 為 Kafka producer（acks=all、idempotent）。
type Producer struct {
	cl *kgo.Client
}

// NewProducer 建立 producer。
func NewProducer(o Options) (*Producer, error) {
	opts, err := o.clientOpts()
	if err != nil {
		return nil, err
	}
	opts = append(opts,
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchCompression(kgo.SnappyCompression()),
		kgo.ProduceRequestTimeout(10*time.Second),
		kgo.RecordRetries(5),
		kgo.AllowAutoTopicCreation(),
	)
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("eventbus: new producer: %w", err)
	}
	return &Producer{cl: cl}, nil
}

// Publish 同步發佈一筆訊息（實作 outbox.Publisher）。
func (p *Producer) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	rec := &kgo.Record{Topic: topic, Key: []byte(key), Value: value, Headers: toHeaders(headers)}
	if err := p.cl.ProduceSync(ctx, rec).FirstErr(); err != nil {
		return fmt.Errorf("eventbus: produce %s: %w", topic, err)
	}
	return nil
}

// Ping 檢查 broker 可達（readiness 用）。
func (p *Producer) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return p.cl.Ping(ctx)
}

// Close flush 並關閉。
func (p *Producer) Close(ctx context.Context) error {
	err := p.cl.Flush(ctx)
	p.cl.Close()
	return err
}

// Record 為交給 handler 的訊息。
type Record struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       string
	Value     []byte
	Headers   map[string]string
	Timestamp time.Time
}

// EventID 取 header 中的 event_id。
func (r Record) EventID() string { return r.Headers[HeaderEventID] }

// Handler 處理一筆訊息；回傳 nil 才會 commit。
type Handler func(ctx context.Context, rec Record) error

// ConsumerConfig 為 consumer 設定。
type ConsumerConfig struct {
	Options
	Group  string
	Topics []string
	// MaxRetries 為 handler 失敗時 in-process 的重試次數（預設 3；100ms/500ms/2s）。
	MaxRetries int
	// DLQ 非 nil 時，重試耗盡的訊息送到 <topic>.dlq 並 commit，不阻塞分區；nil 則停止消費並回傳錯誤。
	DLQ *Producer
}

// Consumer 為 consumer group 成員（手動 commit）。
type Consumer struct {
	cfg ConsumerConfig
	cl  *kgo.Client
	log *slog.Logger
}

// NewConsumer 建立 consumer。
func NewConsumer(cfg ConsumerConfig) (*Consumer, error) {
	opts, err := cfg.clientOpts()
	if err != nil {
		return nil, err
	}
	if cfg.Group == "" {
		return nil, errors.New("eventbus: consumer group is required")
	}
	if len(cfg.Topics) == 0 {
		return nil, errors.New("eventbus: at least one topic is required")
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	opts = append(opts,
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(cfg.Topics...),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.FetchMaxWait(time.Second),
	)
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("eventbus: new consumer: %w", err)
	}
	return &Consumer{cfg: cfg, cl: cl, log: cfg.Logger.With("component", "kafka-consumer", "group", cfg.Group)}, nil
}

// Run 持續消費直到 ctx 結束；每筆訊息 handler 成功後才 commit。
func (c *Consumer) Run(ctx context.Context, handler Handler) error {
	defer c.cl.Close()
	c.log.Info("kafka consumer started", "topics", c.cfg.Topics)
	for {
		if ctx.Err() != nil {
			c.log.Info("kafka consumer stopped")
			return nil
		}
		fetches := c.cl.PollFetches(ctx)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return nil
		}
		var fetchErr error
		fetches.EachError(func(topic string, partition int32, err error) {
			c.log.Error("kafka fetch error", "topic", topic, "partition", partition, "err", err)
			fetchErr = err
		})
		if fetchErr != nil && fetches.NumRecords() == 0 {
			// 暫時性錯誤：稍後重試。
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		var stopErr error
		fetches.EachRecord(func(kr *kgo.Record) {
			if stopErr != nil {
				return
			}
			rec := fromKgo(kr)
			if err := c.handleWithRetry(ctx, handler, rec); err != nil {
				stopErr = err
				return
			}
			if err := c.cl.CommitRecords(ctx, kr); err != nil {
				c.log.Error("kafka commit failed", "topic", kr.Topic, "offset", kr.Offset, "err", err)
			}
		})
		c.cl.AllowRebalance()
		if stopErr != nil {
			return stopErr
		}
	}
}

func (c *Consumer) handleWithRetry(ctx context.Context, handler Handler, rec Record) error {
	delays := []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}
	var err error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if err = handler(ctx, rec); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d := delays[min(attempt, len(delays)-1)]
		c.log.Warn("handler failed, retrying", "topic", rec.Topic, "offset", rec.Offset, "attempt", attempt+1, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	if c.cfg.DLQ == nil {
		return fmt.Errorf("eventbus: handler failed after retries (topic=%s offset=%d): %w", rec.Topic, rec.Offset, err)
	}
	headers := map[string]string{}
	for k, v := range rec.Headers {
		headers[k] = v
	}
	headers[HeaderOriginalTopic] = rec.Topic
	headers[HeaderError] = err.Error()
	if perr := c.cfg.DLQ.Publish(ctx, rec.Topic+DLQSuffix, rec.Key, rec.Value, headers); perr != nil {
		return fmt.Errorf("eventbus: dlq publish: %w", perr)
	}
	c.log.Error("message sent to dlq", "topic", rec.Topic, "offset", rec.Offset, "err", err)
	return nil
}

func fromKgo(kr *kgo.Record) Record {
	h := make(map[string]string, len(kr.Headers))
	for _, kh := range kr.Headers {
		h[kh.Key] = string(kh.Value)
	}
	return Record{Topic: kr.Topic, Partition: kr.Partition, Offset: kr.Offset, Key: string(kr.Key), Value: kr.Value, Headers: h, Timestamp: kr.Timestamp}
}

func toHeaders(h map[string]string) []kgo.RecordHeader {
	if len(h) == 0 {
		return nil
	}
	out := make([]kgo.RecordHeader, 0, len(h))
	for k, v := range h {
		out = append(out, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}
	return out
}

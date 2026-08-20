// ledger-service 進入點：透過 pkg/app 啟動（設定 → logger → OTel → auto migrate → HTTP/gRPC → 優雅關機）。
//
// 組裝：pgdb pool → postgres repos → app.Service → gRPC server；
// 設定了 Kafka broker 時另起 payment.events consumer（Workers）與 outbox relay → ledger.events（PostDrainWorkers）；
// 未設定時只跑 DB + gRPC 並 log warning（本機開發）。
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/tenghongzou/paymentgateway/internal/ledger/adapter/grpc"
	"github.com/tenghongzou/paymentgateway/internal/ledger/adapter/kafka"
	"github.com/tenghongzou/paymentgateway/internal/ledger/adapter/postgres"
	"github.com/tenghongzou/paymentgateway/internal/ledger/app"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	pkgapp "github.com/tenghongzou/paymentgateway/pkg/app"
	"github.com/tenghongzou/paymentgateway/pkg/config"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// 由 -ldflags -X main.version=... 注入。
var version, commit, buildDate = "dev", "none", "unknown"

// Config 為本服務設定（內嵌共用 Base；服務專屬變數加在這裡）。
type Config struct {
	config.Base
	// RefundChargebackFeeOnWin 為 true 時 dispute.won 會退回拒付費（J-CB-WON-FEE，docs/02 §7.3；預設 false）。
	RefundChargebackFeeOnWin bool `env:"PG_LEDGER_REFUND_CHARGEBACK_FEE_ON_WIN" envDefault:"false"`
	// OutboxBatchSize / OutboxPollInterval 為 relay 參數。
	OutboxBatchSize    int           `env:"PG_LEDGER_OUTBOX_BATCH_SIZE" envDefault:"100"`
	OutboxPollInterval time.Duration `env:"PG_LEDGER_OUTBOX_POLL_INTERVAL" envDefault:"500ms"`
}

func main() {
	pkgapp.Run(pkgapp.Options{
		Info:             pkgapp.Info{Version: version, Commit: commit, BuildDate: buildDate},
		MigrationService: "ledger",
	}, setup)
}

func setup(ctx context.Context, rt *pkgapp.Runtime, cfg Config) (*pkgapp.Hooks, error) {
	log := rt.Logger
	hooks := &pkgapp.Hooks{}

	// ---- DB ----
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("ledger-service: PG_DATABASE_URL is required")
	}
	pool, err := pgdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "pgdb", Close: func(context.Context) error { pool.Close(); return nil }})
	hooks.Ready = append(hooks.Ready, pkgapp.Check{Name: "postgres", Fn: func(ctx context.Context) error { return pgdb.Ping(ctx, pool) }})

	// ---- app ----
	svc := app.NewService(app.Deps{
		Tx:       postgres.NewTxRunner(pool),
		Accounts: postgres.NewAccountRepo(pool),
		Journals: postgres.NewJournalRepo(pool),
		Balances: postgres.NewBalanceRepo(pool),
		Inbox:    postgres.NewInbox(),
		Outbox:   postgres.NewOutboxStore(),
		Clock:    app.RealClock{},
		Logger:   log,
		Policy:   domain.Policy{RefundChargebackFeeOnWin: cfg.RefundChargebackFeeOnWin},
	})

	// ---- gRPC ----
	if rt.GRPC != nil {
		grpc.NewServer(svc).Register(rt.GRPC)
	} else {
		log.Warn("PG_GRPC_ADDR not set; LedgerService gRPC is disabled")
	}

	// ---- Kafka（可選）----
	if len(cfg.KafkaBrokers) == 0 {
		log.Warn("PG_KAFKA_BROKERS not set; payment.events consumer and outbox relay are disabled (DB-only mode)")
		return hooks, nil
	}
	busOpts := eventbus.Options{
		Brokers: cfg.KafkaBrokers, ClientID: cfg.ServiceName,
		SASLUsername: cfg.KafkaSASLUsername, SASLPassword: cfg.KafkaSASLPassword, TLS: cfg.KafkaTLS, Logger: log,
	}
	producer, err := eventbus.NewProducer(busOpts)
	if err != nil {
		pool.Close()
		return nil, err
	}
	hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "kafka-producer", Close: producer.Close})
	hooks.Ready = append(hooks.Ready, pkgapp.Check{Name: "kafka", Fn: producer.Ping})

	group := cfg.KafkaConsumerGroup
	if group == "" {
		group = kafka.DefaultGroup
	}
	consumer, err := kafka.NewPaymentConsumer(kafka.Config{
		Brokers: cfg.KafkaBrokers, Group: group, ClientID: cfg.ServiceName,
		SASLUsername: cfg.KafkaSASLUsername, SASLPassword: cfg.KafkaSASLPassword, TLS: cfg.KafkaTLS,
		DLQ: producer, Logger: log,
	}, svc)
	if err != nil {
		pool.Close()
		return nil, err
	}
	hooks.Workers = append(hooks.Workers, pkgapp.Worker{Name: "payment-events-consumer", Run: consumer.Run})

	relay := outbox.NewRelay(outbox.RelayConfig{
		Batcher:      outbox.NewPGBatcher(pool),
		Publisher:    producer,
		Topic:        func(outbox.Message) string { return eventbus.TopicLedgerEvents },
		BatchSize:    cfg.OutboxBatchSize,
		PollInterval: cfg.OutboxPollInterval,
		Logger:       log,
	})
	hooks.PostDrainWorkers = append(hooks.PostDrainWorkers, pkgapp.Worker{Name: "outbox-relay", Run: relay.Run})

	log.Info("ledger-service wired", "kafka_group", group, "topics_in", eventbus.TopicPaymentEvents, "topic_out", eventbus.TopicLedgerEvents)
	return hooks, nil
}

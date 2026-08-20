// reconciliation-service 進入點：透過 pkg/app 啟動（設定 → logger → OTel → auto migrate → HTTP/gRPC → 優雅關機）。
//
// 組裝：DB pool → postgres repos → app.Service → gRPC server；Kafka 有設定時啟動 payment.events consumer 與 outbox relay，
// 未設定時只啟 gRPC 並 log warning（事件會留在 outbox 等 relay 補送）。
package main

import (
	"context"
	"fmt"
	"time"

	grpcadapter "github.com/tenghongzou/paymentgateway/internal/reconciliation/adapter/grpc"
	kafkaadapter "github.com/tenghongzou/paymentgateway/internal/reconciliation/adapter/kafka"
	pgadapter "github.com/tenghongzou/paymentgateway/internal/reconciliation/adapter/postgres"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
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
	// GracePeriod 為 missing_in_psp 寬限期（PSP 延遲結算，docs/05 §9.2 grace_days，預設 3 天）。
	GracePeriod time.Duration `env:"PG_RECON_GRACE_PERIOD" envDefault:"72h"`
	// ImportMaxBytes 為結算檔大小上限（Phase 0：50MB）。
	ImportMaxBytes int64 `env:"PG_RECON_IMPORT_MAX_BYTES" envDefault:"52428800"`
	// ImportAllowedDirs 限制 file:// 來源可讀取的目錄（逗號分隔；空表示不限制，僅限本機開發）。
	ImportAllowedDirs []string `env:"PG_RECON_IMPORT_ALLOWED_DIRS" envSeparator:","`
	// OutboxBatchSize / OutboxPollInterval 為 relay 參數。
	OutboxBatchSize    int           `env:"PG_OUTBOX_BATCH_SIZE" envDefault:"100"`
	OutboxPollInterval time.Duration `env:"PG_OUTBOX_POLL_INTERVAL" envDefault:"500ms"`
}

func main() {
	pkgapp.Run(pkgapp.Options{
		Info:             pkgapp.Info{Version: version, Commit: commit, BuildDate: buildDate},
		MigrationService: "reconciliation",
	}, setup)
}

func setup(ctx context.Context, rt *pkgapp.Runtime, cfg Config) (*pkgapp.Hooks, error) {
	log := rt.Logger
	hooks := &pkgapp.Hooks{}

	// ---- DB ----
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("PG_DATABASE_URL is required")
	}
	pool, err := pgdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "postgres", Close: func(context.Context) error { pool.Close(); return nil }})
	hooks.Ready = append(hooks.Ready, pkgapp.Check{Name: "postgres", Fn: func(ctx context.Context) error { return pgdb.Ping(ctx, pool) }})

	// ---- app ----
	repos := pgadapter.NewRepos(pool)
	deps := repos.Deps()
	deps.Logger = log
	svc := app.NewService(deps, app.Config{GracePeriod: cfg.GracePeriod})

	// ---- gRPC ----
	if rt.GRPC != nil {
		srv := grpcadapter.NewServer(svc, grpcadapter.Options{
			MaxFileBytes: cfg.ImportMaxBytes,
			Fetcher:      grpcadapter.NewFetcher(grpcadapter.FetcherOptions{MaxBytes: cfg.ImportMaxBytes, AllowedDirs: cfg.ImportAllowedDirs}),
			Logger:       log,
		})
		srv.Register(rt.GRPC)
	} else {
		log.Warn("PG_GRPC_ADDR not set; ReconciliationService gRPC is disabled")
	}

	// ---- Kafka（consumer + outbox relay）----
	if len(cfg.KafkaBrokers) == 0 {
		log.Warn("PG_KAFKA_BROKERS not set; payment.events consumer and outbox relay are disabled (events stay in outbox)")
		return hooks, nil
	}
	busOpts := eventbus.Options{
		Brokers: cfg.KafkaBrokers, ClientID: cfg.ServiceName,
		SASLUsername: cfg.KafkaSASLUsername, SASLPassword: cfg.KafkaSASLPassword, TLS: cfg.KafkaTLS, Logger: log,
	}
	producer, err := eventbus.NewProducer(busOpts)
	if err != nil {
		return nil, err
	}
	hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "kafka-producer", Close: producer.Close})
	hooks.Ready = append(hooks.Ready, pkgapp.Check{Name: "kafka", Fn: producer.Ping})

	group := cfg.KafkaConsumerGroup
	if group == "" {
		group = kafkaadapter.DefaultGroup
	}
	consumer, err := kafkaadapter.NewConsumer(kafkaadapter.Config{Options: busOpts, Group: group, DLQ: producer}, svc)
	if err != nil {
		return nil, err
	}
	hooks.Workers = append(hooks.Workers, pkgapp.Worker{Name: "payment-events-consumer", Run: consumer.Run})

	relay := outbox.NewRelay(outbox.RelayConfig{
		Batcher:      outbox.NewPGBatcher(pool),
		Publisher:    producer,
		Topic:        func(outbox.Message) string { return eventbus.TopicReconciliationEvents },
		BatchSize:    cfg.OutboxBatchSize,
		PollInterval: cfg.OutboxPollInterval,
		Logger:       log,
	})
	hooks.PostDrainWorkers = append(hooks.PostDrainWorkers, pkgapp.Worker{Name: "outbox-relay", Run: relay.Run})
	return hooks, nil
}

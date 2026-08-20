// payment-service 進入點：Payment / Refund 聚合與狀態機、路由與 failover、呼叫 ProviderAdapter、outbox relay（gRPC :9002，DB pg_payment）。
package main

import (
	"context"
	"errors"
	"strings"

	grpcadapter "github.com/tenghongzou/paymentgateway/internal/payment/adapter/grpc"
	pgadapter "github.com/tenghongzou/paymentgateway/internal/payment/adapter/postgres"
	provideradapter "github.com/tenghongzou/paymentgateway/internal/payment/adapter/provider"
	"github.com/tenghongzou/paymentgateway/internal/payment/app"
	pkgapp "github.com/tenghongzou/paymentgateway/pkg/app"
	"github.com/tenghongzou/paymentgateway/pkg/config"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// 由 -ldflags -X main.version=... 注入。
var version, commit, buildDate = "dev", "none", "unknown"

// Config 為 payment-service 設定。
type Config struct {
	config.Base
	// RoutingDefaultOrder 為平台預設路由順序（逗號分隔；預設 mock 優先）。
	RoutingDefaultOrder []string `env:"PG_ROUTING_DEFAULT_ORDER" envSeparator:","`
	// RetrySameProviderOnUnavailable：無其他候選時允許同 Provider 以新 Attempt 重試一次（Phase 0 預設開啟）。
	RetrySameProviderOnUnavailable bool `env:"PG_PROVIDER_RETRY_SAME_ON_UNAVAILABLE" envDefault:"true"`
	// OutboxDisabled 為 true 時不啟動 relay（沒有 Kafka 的本機測試）。
	OutboxDisabled bool `env:"PG_OUTBOX_DISABLED" envDefault:"false"`
}

func main() {
	pkgapp.Run(pkgapp.Options{Info: pkgapp.Info{Version: version, Commit: commit, BuildDate: buildDate}, MigrationService: "payment"}, setup)
}

func setup(ctx context.Context, rt *pkgapp.Runtime, cfg Config) (*pkgapp.Hooks, error) {
	hooks := &pkgapp.Hooks{}
	log := rt.Logger

	if cfg.DatabaseURL == "" {
		return nil, errors.New("PG_DATABASE_URL is required")
	}
	pool, err := pgdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "db-pool", Close: func(context.Context) error { pool.Close(); return nil }})
	hooks.Ready = append(hooks.Ready, pkgapp.Check{Name: "postgres", Fn: func(ctx context.Context) error { return pgdb.Ping(ctx, pool) }})

	if len(cfg.ProviderAddrs) == 0 {
		return nil, errors.New("PG_PROVIDER_ADDRS is required (e.g. mock=provider-mock:9101)")
	}
	registry, err := provideradapter.NewRegistry(ctx, cfg.ProviderAddrs)
	if err != nil {
		return nil, err
	}
	hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "provider-conns", Close: func(context.Context) error { return registry.Close() }})
	router := provideradapter.NewRouter(registry, provideradapter.RouterConfig{DefaultOrder: cfg.RoutingDefaultOrder, Logger: log})

	svc := app.NewService(app.Deps{
		Repo: pgadapter.NewRepo(pool), Tx: pgadapter.NewTxManager(pool), Outbox: pgadapter.NewOutboxStore(),
		Providers: registry, Router: router, Logger: log,
		Config: app.Config{MaxAttempts: cfg.ProviderFailoverMaxAttempts, ProviderTimeout: cfg.ProviderTimeout, RetrySameProviderOnUnavailable: cfg.RetrySameProviderOnUnavailable},
	})
	grpcadapter.NewServer(svc).Register(rt.GRPC)

	// outbox relay → Kafka（payment.events；key = payment_id；value = PaymentEvent protobuf）。
	if !cfg.OutboxDisabled {
		if len(cfg.KafkaBrokers) == 0 {
			return nil, errors.New("PG_KAFKA_BROKERS is required (or set PG_OUTBOX_DISABLED=true)")
		}
		producer, err := eventbus.NewProducer(eventbus.Options{Brokers: cfg.KafkaBrokers, ClientID: cfg.ServiceName, SASLUsername: cfg.KafkaSASLUsername, SASLPassword: cfg.KafkaSASLPassword, TLS: cfg.KafkaTLS, Logger: log})
		if err != nil {
			return nil, err
		}
		hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "kafka-producer", Close: producer.Close})
		hooks.Ready = append(hooks.Ready, pkgapp.Check{Name: "kafka", Fn: producer.Ping})
		relay := outbox.NewRelay(outbox.RelayConfig{
			Batcher: outbox.NewPGBatcher(pool), Publisher: producer, Logger: log,
			Topic: func(m outbox.Message) string {
				if strings.HasPrefix(m.EventType, "refund.") {
					return eventbus.TopicRefundEvents
				}
				return eventbus.TopicPaymentEvents
			},
		})
		hooks.PostDrainWorkers = append(hooks.PostDrainWorkers, pkgapp.Worker{Name: "outbox-relay", Run: relay.Run})
	} else {
		log.Warn("outbox relay disabled (PG_OUTBOX_DISABLED=true); events stay in the outbox table")
	}
	// TODO(payment-service): expire / auth-expiry sweeper、operation-reconciler、refund-reconciler、provider webhook ingestion。
	return hooks, nil
}

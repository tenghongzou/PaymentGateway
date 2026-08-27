// webhook-service 進入點：透過 pkg/app 啟動（設定 → logger → OTel → auto migrate → HTTP/gRPC → 優雅關機）。
//
// 組裝：pg_webhook 連線池 → repos → merchant-service client（端點 + secret，TTL 快取 60s）→ HTTP sender（SSRF 防護）
// → app.Service → gRPC server、Kafka consumer（payment.events）、dispatcher worker、reaper worker。
//
// 子命令：
//
//	webhook-service migrate up|down [N]|version   （pkg/app 提供）
//	webhook-service sink -addr :8099 -secret whsec_xxx  本機 webhook 接收端（internal/webhook/devsink）
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	grpcadapter "github.com/tenghongzou/paymentgateway/internal/webhook/adapter/grpc"
	httpadapter "github.com/tenghongzou/paymentgateway/internal/webhook/adapter/http"
	kafkaadapter "github.com/tenghongzou/paymentgateway/internal/webhook/adapter/kafka"
	pgadapter "github.com/tenghongzou/paymentgateway/internal/webhook/adapter/postgres"
	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/devsink"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	pkgapp "github.com/tenghongzou/paymentgateway/pkg/app"
	"github.com/tenghongzou/paymentgateway/pkg/config"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// 由 -ldflags -X main.version=... 注入。
var version, commit, buildDate = "dev", "none", "unknown"

// Config 為本服務設定（內嵌共用 Base；服務專屬變數加在這裡）。
type Config struct {
	config.Base

	// Timeout / ConnectTimeout 為單次投遞逾時（docs/06 §4.4：整體 10s、連線 3s）。
	Timeout        time.Duration `env:"PG_WEBHOOK_TIMEOUT" envDefault:"10s"`
	ConnectTimeout time.Duration `env:"PG_WEBHOOK_CONNECT_TIMEOUT" envDefault:"3s"`
	// dispatcher：輪詢間隔、每批取件數、並發投遞數。
	DispatchInterval    time.Duration `env:"PG_WEBHOOK_DISPATCH_INTERVAL" envDefault:"500ms"`
	DispatchBatch       int           `env:"PG_WEBHOOK_DISPATCH_BATCH" envDefault:"50"`
	DispatchConcurrency int           `env:"PG_WEBHOOK_DISPATCH_CONCURRENCY" envDefault:"16"`
	// reaper：in_flight 逾時與掃描間隔。
	InFlightTimeout time.Duration `env:"PG_WEBHOOK_IN_FLIGHT_TIMEOUT" envDefault:"2m"`
	ReaperInterval  time.Duration `env:"PG_WEBHOOK_REAPER_INTERVAL" envDefault:"30s"`
	// 端點快取 TTL。
	EndpointCacheTTL time.Duration `env:"PG_WEBHOOK_ENDPOINT_CACHE_TTL" envDefault:"60s"`
	// AllowInsecureURLs：允許 http / 私有位址（SSRF 政策 dev 模式）。空值時 dev 環境為 true、其他為 false。
	AllowInsecureURLs string `env:"PG_WEBHOOK_ALLOW_INSECURE_URLS"`
	// 開發用靜態端點（未設定 PG_MERCHANT_SERVICE_ADDR 且為 dev 時使用），搭配 `webhook-service sink`。
	DevEndpointURL    string `env:"PG_WEBHOOK_DEV_ENDPOINT_URL"`
	DevEndpointSecret string `env:"PG_WEBHOOK_DEV_ENDPOINT_SECRET"`
	// KafkaDLQ：handler 重試耗盡時把訊息送到 payment.events.dlq（需 producer）。
	KafkaDLQ bool `env:"PG_WEBHOOK_KAFKA_DLQ" envDefault:"true"`
}

// Validate 補充本服務的設定檢查。
func (c Config) Validate() error {
	if err := c.Base.Validate(); err != nil {
		return err
	}
	switch strings.ToLower(c.AllowInsecureURLs) {
	case "", "true", "false", "1", "0":
	default:
		return fmt.Errorf("config: PG_WEBHOOK_ALLOW_INSECURE_URLS must be true|false, got %q", c.AllowInsecureURLs)
	}
	return nil
}

func (c Config) allowInsecure() bool {
	switch strings.ToLower(c.AllowInsecureURLs) {
	case "true", "1":
		return true
	case "false", "0":
		return false
	default:
		return c.IsDev()
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sink" {
		os.Exit(devsink.Main(os.Args[2:]))
	}
	pkgapp.Run(pkgapp.Options{
		Info:             pkgapp.Info{Version: version, Commit: commit, BuildDate: buildDate},
		MigrationService: "webhook",
	}, setup)
}

func setup(ctx context.Context, rt *pkgapp.Runtime, cfg Config) (*pkgapp.Hooks, error) {
	log := rt.Logger
	hooks := &pkgapp.Hooks{}

	// ---- DB ----
	if cfg.DatabaseURL == "" {
		return nil, errors.New("PG_DATABASE_URL is required")
	}
	pool, err := pgdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	store := pgadapter.NewStore(pool)
	hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "pg_webhook pool", Close: store.Close})
	hooks.Ready = append(hooks.Ready, pkgapp.Check{Name: "postgres", Fn: store.Ping})

	// ---- SSRF 政策 ----
	policy := domain.StrictPolicy
	if cfg.allowInsecure() {
		policy = domain.DevPolicy
		if !cfg.IsDev() {
			log.Warn("PG_WEBHOOK_ALLOW_INSECURE_URLS=true outside dev: SSRF protection is disabled")
		}
	}

	// ---- 端點來源（merchant-service）----
	var (
		endpointSrc app.EndpointSource
		disabler    app.EndpointDisabler
	)
	switch {
	case cfg.MerchantServiceAddr != "":
		conn, dialErr := grpcx.Dial(ctx, cfg.MerchantServiceAddr)
		if dialErr != nil {
			return nil, dialErr
		}
		hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "merchant-service conn", Close: func(context.Context) error { return conn.Close() }})
		ms := grpcadapter.NewMerchantEndpointSource(conn, nil)
		endpointSrc, disabler = ms, ms
	case cfg.IsDev():
		static := &app.StaticEndpointSource{}
		if cfg.DevEndpointURL != "" {
			static.Endpoints = []*domain.Endpoint{{
				ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(cfg.DevEndpointURL)), URL: cfg.DevEndpointURL,
				Secrets: []string{cfg.DevEndpointSecret}, Status: domain.EndpointEnabled, Livemode: false, EnabledEvents: []string{"*"},
			}}
			// dev 靜態端點同時接收 live / test 事件：再放一份 livemode=true。
			live := *static.Endpoints[0]
			live.ID = uuid.NewSHA1(uuid.NameSpaceURL, []byte(cfg.DevEndpointURL+"#live"))
			live.Livemode = true
			static.Endpoints = append(static.Endpoints, &live)
			log.Warn("PG_MERCHANT_SERVICE_ADDR not set; using static dev endpoint for all merchants", "url", cfg.DevEndpointURL)
		} else {
			log.Warn("PG_MERCHANT_SERVICE_ADDR not set and no PG_WEBHOOK_DEV_ENDPOINT_URL; no webhooks will be delivered")
		}
		endpointSrc = static
	default:
		return nil, errors.New("PG_MERCHANT_SERVICE_ADDR is required")
	}
	endpoints := app.NewEndpointCache(endpointSrc, cfg.EndpointCacheTTL, nil)

	// ---- HTTP sender ----
	sender := httpadapter.NewSender(httpadapter.Options{Policy: policy, Timeout: cfg.Timeout, ConnectTimeout: cfg.ConnectTimeout})

	// ---- use cases ----
	svc := app.New(app.Deps{
		Tx: store, Inbox: pgadapter.NewInbox(), Events: pgadapter.NewEventRepo(store), Deliveries: pgadapter.NewDeliveryRepo(store),
		Endpoints: endpoints, Disabler: disabler, Sender: sender, Policy: policy, Logger: log,
	})

	// ---- gRPC ----
	if rt.GRPC != nil {
		grpcadapter.NewServer(svc).Register(rt.GRPC)
	}

	// ---- workers ----
	hooks.Workers = append(hooks.Workers,
		pkgapp.Worker{Name: "webhook-dispatcher", Run: (&app.Dispatcher{
			Svc: svc, Interval: cfg.DispatchInterval, Batch: cfg.DispatchBatch, Concurrency: cfg.DispatchConcurrency, Logger: log,
		}).Run},
		pkgapp.Worker{Name: "webhook-reaper", Run: (&app.Reaper{
			Svc: svc, Interval: cfg.ReaperInterval, Timeout: cfg.InFlightTimeout, Logger: log,
		}).Run},
	)

	// ---- Kafka consumer ----
	if len(cfg.KafkaBrokers) == 0 {
		log.Warn("PG_KAFKA_BROKERS not set; payment.events consumer is disabled")
	} else {
		opts := eventbus.Options{
			Brokers: cfg.KafkaBrokers, ClientID: cfg.ServiceName, SASLUsername: cfg.KafkaSASLUsername,
			SASLPassword: cfg.KafkaSASLPassword, TLS: cfg.KafkaTLS, Logger: log,
		}
		var dlq *eventbus.Producer
		if cfg.KafkaDLQ {
			dlq, err = eventbus.NewProducer(opts)
			if err != nil {
				return nil, err
			}
			hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "kafka dlq producer", Close: dlq.Close})
		}
		group := cfg.KafkaConsumerGroup
		if group == "" {
			group = kafkaadapter.DefaultGroup
		}
		consumer, err := kafkaadapter.NewConsumer(kafkaadapter.Config{Options: opts, Group: group, DLQ: dlq}, svc, log)
		if err != nil {
			return nil, err
		}
		hooks.Workers = append(hooks.Workers, pkgapp.Worker{Name: "payment.events consumer", Run: consumer.Run})
	}

	log.Info("webhook-service configured",
		"dispatch_interval", cfg.DispatchInterval, "dispatch_batch", cfg.DispatchBatch, "dispatch_concurrency", cfg.DispatchConcurrency,
		"in_flight_timeout", cfg.InFlightTimeout, "endpoint_cache_ttl", cfg.EndpointCacheTTL, "insecure_urls", policy.AllowInsecure,
		"max_attempts", domain.MaxAttempts)
	return hooks, nil
}

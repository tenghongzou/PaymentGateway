// merchant-service 進入點：透過 pkg/app 啟動（設定 → logger → OTel → auto migrate → HTTP/gRPC → outbox relay → 優雅關機）。
//
// 子命令：
//
//	merchant-service migrate up|down [N]|version   （pkg/app 內建）
//	merchant-service seed-dev                      建立 dev 商戶 + 一把 test key，印出 PG_DEV_* 給 api-gateway 開發模式
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	grpcadapter "github.com/tenghongzou/paymentgateway/internal/merchant/adapter/grpc"
	"github.com/tenghongzou/paymentgateway/internal/merchant/adapter/postgres"
	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
	"github.com/tenghongzou/paymentgateway/migrations"
	pkgapp "github.com/tenghongzou/paymentgateway/pkg/app"
	"github.com/tenghongzou/paymentgateway/pkg/config"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// 由 -ldflags -X main.version=... 注入。
var version, commit, buildDate = "dev", "none", "unknown"

const migrationService = "merchant"

// Config 為本服務設定（內嵌共用 Base；服務專屬變數加在這裡）。
type Config struct {
	config.Base
	// KEK 為 signing secret / webhook secret 欄位加密的 AES-256 金鑰（base64 32 bytes）。
	// dev 未設定時退回 plaintext-with-prefix 並印 warning；staging / production 必填（fail closed）。
	// Phase 1 改為 Vault transit（docs/06 §7.3）。
	KEK string `env:"PG_KEK"`
	// KnownProviders 為路由規則允許的 provider 名稱。
	KnownProviders []string `env:"PG_ROUTING_KNOWN_PROVIDERS" envSeparator:"," envDefault:"mock,stripe"`
	// WebhookURLAllowInsecure 允許 http / localhost / 私有 IP 的 webhook URL（dev 預設開）。
	WebhookURLAllowInsecure bool `env:"PG_WEBHOOK_URL_ALLOW_INSECURE" envDefault:"false"`
	// WebhookURLResolveDNS 在非 insecure 模式下解析 DNS 並拒絕私有網段（docs/06 §4.5）。
	WebhookURLResolveDNS bool `env:"PG_WEBHOOK_URL_RESOLVE_DNS" envDefault:"true"`
	// Outbox relay 參數。
	OutboxBatchSize    int           `env:"PG_OUTBOX_BATCH_SIZE" envDefault:"100"`
	OutboxPollInterval time.Duration `env:"PG_OUTBOX_POLL_INTERVAL" envDefault:"500ms"`
}

// Validate 在 Base.Validate 之外檢查本服務的必填項。
func (c Config) Validate() error {
	if err := c.Base.Validate(); err != nil {
		return err
	}
	if c.DatabaseURL == "" {
		return errors.New("config: PG_DATABASE_URL is required for merchant-service")
	}
	if c.KEK == "" && !c.IsDev() {
		return errors.New("config: PG_KEK is required outside dev (secrets must not be stored in plaintext)")
	}
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "seed-dev" {
		os.Exit(runSeedDev())
	}
	pkgapp.Run(pkgapp.Options{
		Info:             pkgapp.Info{Version: version, Commit: commit, BuildDate: buildDate},
		MigrationService: migrationService,
	}, setup)
}

// setup 組裝依賴並註冊 gRPC 服務、readiness、outbox relay 與 closers。
func setup(ctx context.Context, rt *pkgapp.Runtime, cfg Config) (*pkgapp.Hooks, error) {
	log := rt.Logger
	pool, err := pgdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	svc, err := buildService(pool, cfg, log)
	if err != nil {
		pool.Close()
		return nil, err
	}
	grpcadapter.NewServer(svc, app.SystemClock{}).Register(rt.GRPC)

	hooks := &pkgapp.Hooks{
		Ready: []pkgapp.Check{{Name: "postgres", Fn: func(ctx context.Context) error { return pgdb.Ping(ctx, pool) }}},
		Closers: []pkgapp.Closer{
			{Name: "merchant-service", Close: svc.Close},
		},
	}

	// ---- outbox relay → Kafka merchant.events ----
	if len(cfg.KafkaBrokers) > 0 {
		producer, err := eventbus.NewProducer(eventbus.Options{
			Brokers: cfg.KafkaBrokers, ClientID: cfg.ServiceName, SASLUsername: cfg.KafkaSASLUsername, SASLPassword: cfg.KafkaSASLPassword, TLS: cfg.KafkaTLS, Logger: log,
		})
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("kafka producer: %w", err)
		}
		relay := outbox.NewRelay(outbox.RelayConfig{
			Batcher:      outbox.NewPGBatcher(pool),
			Publisher:    producer,
			Topic:        func(outbox.Message) string { return eventbus.TopicMerchantEvents },
			BatchSize:    cfg.OutboxBatchSize,
			PollInterval: cfg.OutboxPollInterval,
			Logger:       log,
		})
		hooks.Ready = append(hooks.Ready, pkgapp.Check{Name: "kafka", Fn: producer.Ping})
		hooks.PostDrainWorkers = append(hooks.PostDrainWorkers, pkgapp.Worker{Name: "outbox-relay", Run: relay.Run})
		hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "kafka-producer", Close: producer.Close})
	} else {
		log.Warn("PG_KAFKA_BROKERS not set: outbox relay disabled, merchant.events will accumulate in the outbox table")
	}
	hooks.Closers = append(hooks.Closers, pkgapp.Closer{Name: "postgres-pool", Close: func(context.Context) error {
		pool.Close()
		return nil
	}})
	return hooks, nil
}

// buildService 建立 app.Service（repos、cipher、URL policy）。
func buildService(pool *pgxpool.Pool, cfg Config, log *slog.Logger) (*app.Service, error) {
	cipher, err := newCipher(cfg, log)
	if err != nil {
		return nil, err
	}
	repos := postgres.NewRepos(pool)
	allowInsecure := cfg.WebhookURLAllowInsecure || cfg.IsDev()
	var resolver domain.Resolver
	if !allowInsecure && cfg.WebhookURLResolveDNS {
		resolver = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, nil
		}
	}
	return app.New(app.Deps{
		Tx: repos.Tx, Merchants: repos.Merchants, APIKeys: repos.APIKeys, Webhooks: repos.Webhooks, Routing: repos.Routing, Outbox: repos.Outbox,
		Clock: app.SystemClock{}, Cipher: cipher, Logger: log,
	}, app.Config{
		AllowInsecureWebhookURL: allowInsecure,
		URLResolver:             resolver,
		KnownProviders:          cfg.KnownProviders,
	})
}

// newCipher 依 PG_KEK 選擇欄位加密實作。
func newCipher(cfg Config, log *slog.Logger) (domain.SecretCipher, error) {
	if cfg.KEK != "" {
		c, err := domain.NewAESGCMCipherFromBase64(cfg.KEK)
		if err != nil {
			return nil, fmt.Errorf("PG_KEK: %w", err)
		}
		log.Info("secret cipher: AES-256-GCM with PG_KEK (Phase 0 envelope-lite; Phase 1: Vault transit)")
		return c, nil
	}
	if !cfg.IsDev() {
		return nil, errors.New("PG_KEK is required outside dev")
	}
	log.Warn("SECURITY WARNING: PG_KEK not set; signing secrets and webhook secrets will be stored as PLAINTEXT (plain:v1:) in pg_merchant. " +
		"This is only acceptable for local dev. Set PG_KEK=$(openssl rand -base64 32).")
	return domain.PlaintextCipher{}, nil
}

// runSeedDev 實作 `merchant-service seed-dev`：套用 migration、建立 dev 商戶 + test key，印出環境變數。
func runSeedDev() int {
	cfg, err := config.Load[Config]()
	if err != nil {
		slog.Error("load config", "err", err)
		return 2
	}
	log := logx.New(cfg.ServiceName, cfg.Env, cfg.LogLevel)
	slog.SetDefault(log)
	if !cfg.IsDev() {
		log.Error("seed-dev is only available when PG_ENV=dev")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src, err := migrations.Source(migrationService)
	if err != nil {
		log.Error("migration source", "err", err)
		return 1
	}
	if err := pgdb.Migrate(ctx, cfg.EffectiveMigrateURL(), migrationService, src); err != nil {
		log.Error("migrate up", "err", err)
		return 1
	}
	pool, err := pgdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("connect db", "err", err)
		return 1
	}
	defer pool.Close()
	svc, err := buildService(pool, cfg, log)
	if err != nil {
		log.Error("build service", "err", err)
		return 1
	}
	res, err := svc.SeedDev(ctx)
	if err != nil {
		log.Error("seed-dev failed", "err", err)
		return 1
	}
	log.Info("dev merchant seeded", "merchant_id", res.MerchantID, "api_key_id", res.APIKeyID, "reused_merchant", res.Reused)
	// 明文只印到 stdout（不進 log），方便 `eval "$(merchant-service seed-dev 2>/dev/null)"`。
	fmt.Fprintf(os.Stdout, "PG_DEV_MERCHANT_ID=%s\nPG_DEV_API_KEY=%s\nPG_DEV_SIGNING_SECRET=%s\n", res.MerchantID, res.APIKey, res.SigningSecret)
	return 0
}

// Package config 以環境變數（PG_ 前綴，caarlos0/env）載入服務設定。
//
// 每個服務定義自己的 Config struct 並內嵌 Base，再以 Load[T]() 載入；
// 未知的 PG_* 變數會被忽略（docs/07 §1.8 第 8 點）。
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// 環境名稱。
const (
	EnvDev        = "dev"
	EnvStaging    = "staging"
	EnvProduction = "production"
)

// Base 為所有服務共用的設定（docs/07 §2 環境變數總表）。
type Base struct {
	ServiceName string `env:"PG_SERVICE_NAME" envDefault:"unknown"`
	Env         string `env:"PG_ENV" envDefault:"dev"`
	LogLevel    string `env:"PG_LOG_LEVEL" envDefault:"info"`

	HTTPAddr string `env:"PG_HTTP_ADDR" envDefault:":8081"`
	GRPCAddr string `env:"PG_GRPC_ADDR"`

	DatabaseURL        string `env:"PG_DATABASE_URL"`
	MigrateDatabaseURL string `env:"PG_MIGRATE_DATABASE_URL"`
	AutoMigrate        bool   `env:"PG_AUTO_MIGRATE" envDefault:"false"`
	// MigrationsDir 保留給運維契約（映像內為 /migrations）；程式實際使用 migrations 套件的 embed.FS。
	MigrationsDir string `env:"PG_MIGRATIONS_DIR" envDefault:"/migrations"`

	ValkeyAddr     string `env:"PG_VALKEY_ADDR"`
	ValkeyPassword string `env:"PG_VALKEY_PASSWORD"`

	KafkaBrokers       []string `env:"PG_KAFKA_BROKERS" envSeparator:","`
	KafkaConsumerGroup string   `env:"PG_KAFKA_CONSUMER_GROUP"`
	KafkaSASLUsername  string   `env:"PG_KAFKA_SASL_USERNAME"`
	KafkaSASLPassword  string   `env:"PG_KAFKA_SASL_PASSWORD"`
	KafkaTLS           bool     `env:"PG_KAFKA_TLS" envDefault:"false"`

	OTelExporterOTLPEndpoint string `env:"PG_OTEL_EXPORTER_OTLP_ENDPOINT"`
	OTelExporterOTLPInsecure bool   `env:"PG_OTEL_EXPORTER_OTLP_INSECURE" envDefault:"false"`

	MerchantServiceAddr       string `env:"PG_MERCHANT_SERVICE_ADDR"`
	PaymentServiceAddr        string `env:"PG_PAYMENT_SERVICE_ADDR"`
	LedgerServiceAddr         string `env:"PG_LEDGER_SERVICE_ADDR"`
	WebhookServiceAddr        string `env:"PG_WEBHOOK_SERVICE_ADDR"`
	ReconciliationServiceAddr string `env:"PG_RECONCILIATION_SERVICE_ADDR"`

	// ProviderAddrs 由 "mock=host:port,stripe=host:port" 解析而來。
	ProviderAddrs               map[string]string `env:"PG_PROVIDER_ADDRS" envSeparator:"," envKeyValSeparator:"="`
	ProviderTimeout             time.Duration     `env:"PG_PROVIDER_TIMEOUT" envDefault:"10s"`
	ProviderFailoverMaxAttempts int               `env:"PG_PROVIDER_FAILOVER_MAX_ATTEMPTS" envDefault:"2"`

	RateLimitRPS   int           `env:"PG_RATE_LIMIT_RPS" envDefault:"100"`
	IdempotencyTTL time.Duration `env:"PG_IDEMPOTENCY_TTL" envDefault:"24h"`

	ShutdownTimeout time.Duration `env:"PG_SHUTDOWN_TIMEOUT" envDefault:"20s"`
}

// BaseConfig 讓內嵌 Base 的服務設定都能被 pkg/app 取得共用欄位。
func (b Base) BaseConfig() Base { return b }

// IsDev 回傳是否為本機開發環境。
func (b Base) IsDev() bool { return b.Env == EnvDev }

// IsProduction 回傳是否為正式環境。
func (b Base) IsProduction() bool { return b.Env == EnvProduction }

// EffectiveMigrateURL 回傳 migration 用的連線字串（空則退回 DatabaseURL）。
func (b Base) EffectiveMigrateURL() string {
	if b.MigrateDatabaseURL != "" {
		return b.MigrateDatabaseURL
	}
	return b.DatabaseURL
}

// Validate 做最基本的格式檢查。
func (b Base) Validate() error {
	switch b.Env {
	case EnvDev, EnvStaging, EnvProduction:
	default:
		return fmt.Errorf("config: PG_ENV must be dev|staging|production, got %q", b.Env)
	}
	switch strings.ToLower(b.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: PG_LOG_LEVEL must be debug|info|warn|error, got %q", b.LogLevel)
	}
	for name, addr := range b.ProviderAddrs {
		if name == "" || addr == "" {
			return fmt.Errorf("config: PG_PROVIDER_ADDRS has empty entry (%q=%q)", name, addr)
		}
	}
	return nil
}

// Load 以環境變數載入 T（T 通常內嵌 Base）。
func Load[T any]() (T, error) {
	var cfg T
	if err := env.Parse(&cfg); err != nil {
		return cfg, fmt.Errorf("config: parse env: %w", err)
	}
	if v, ok := any(cfg).(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// LoadFrom 以指定的 key/value 載入（測試用，不讀 os.Environ）。
func LoadFrom[T any](values map[string]string) (T, error) {
	var cfg T
	if err := env.ParseWithOptions(&cfg, env.Options{Environment: values}); err != nil {
		return cfg, fmt.Errorf("config: parse env: %w", err)
	}
	if v, ok := any(cfg).(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

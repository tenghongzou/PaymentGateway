package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type svcConfig struct {
	Base
	DevAPIKey string `env:"PG_DEV_API_KEY"`
}

func TestLoadFromDefaults(t *testing.T) {
	cfg, err := LoadFrom[Base](map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "unknown", cfg.ServiceName)
	assert.Equal(t, EnvDev, cfg.Env)
	assert.Equal(t, ":8081", cfg.HTTPAddr)
	assert.Equal(t, 20*time.Second, cfg.ShutdownTimeout)
	assert.Equal(t, 10*time.Second, cfg.ProviderTimeout)
	assert.Equal(t, 2, cfg.ProviderFailoverMaxAttempts)
	assert.Equal(t, 24*time.Hour, cfg.IdempotencyTTL)
	assert.False(t, cfg.AutoMigrate)
	assert.True(t, cfg.IsDev())
}

func TestLoadFromFull(t *testing.T) {
	cfg, err := LoadFrom[svcConfig](map[string]string{
		"PG_SERVICE_NAME":         "payment-service",
		"PG_ENV":                  "staging",
		"PG_KAFKA_BROKERS":        "a:9092,b:9092",
		"PG_PROVIDER_ADDRS":       "mock=provider-mock:9101,stripe=provider-stripe:9102",
		"PG_DATABASE_URL":         "postgres://app@db/pg_payment",
		"PG_MIGRATE_DATABASE_URL": "",
		"PG_SHUTDOWN_TIMEOUT":     "5s",
		"PG_DEV_API_KEY":          "pk_test_x",
		"PG_UNKNOWN_THING":        "ignored",
	})
	require.NoError(t, err)
	assert.Equal(t, "payment-service", cfg.ServiceName)
	assert.Equal(t, []string{"a:9092", "b:9092"}, cfg.KafkaBrokers)
	assert.Equal(t, map[string]string{"mock": "provider-mock:9101", "stripe": "provider-stripe:9102"}, cfg.ProviderAddrs)
	assert.Equal(t, "postgres://app@db/pg_payment", cfg.EffectiveMigrateURL())
	assert.Equal(t, 5*time.Second, cfg.ShutdownTimeout)
	assert.Equal(t, "pk_test_x", cfg.DevAPIKey)
	assert.Equal(t, "payment-service", cfg.BaseConfig().ServiceName)
}

func TestLoadFromInvalid(t *testing.T) {
	_, err := LoadFrom[Base](map[string]string{"PG_ENV": "qa"})
	require.Error(t, err)
	_, err = LoadFrom[Base](map[string]string{"PG_LOG_LEVEL": "trace"})
	require.Error(t, err)
	_, err = LoadFrom[Base](map[string]string{"PG_SHUTDOWN_TIMEOUT": "abc"})
	require.Error(t, err)
}

func TestLoadFromOSEnv(t *testing.T) {
	t.Setenv("PG_SERVICE_NAME", "x-service")
	cfg, err := Load[Base]()
	require.NoError(t, err)
	assert.Equal(t, "x-service", cfg.ServiceName)
}

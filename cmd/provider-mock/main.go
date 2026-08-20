// provider-mock 進入點：模擬 PSP（docs/09 §3），無 DB。
package main

import (
	"context"
	"time"

	"github.com/tenghongzou/paymentgateway/internal/providermock"
	"github.com/tenghongzou/paymentgateway/pkg/app"
	"github.com/tenghongzou/paymentgateway/pkg/config"
)

// 由 -ldflags -X main.version=... 注入。
var version, commit, buildDate = "dev", "none", "unknown"

// Config 為 provider-mock 設定。
type Config struct {
	config.Base
	DefaultLatency time.Duration `env:"PG_MOCK_DEFAULT_LATENCY" envDefault:"50ms"`
	FailureRate    float64       `env:"PG_MOCK_FAILURE_RATE" envDefault:"0"` // TODO: 全域失敗率注入（混沌測試）
	WebhookSecret  string        `env:"PG_MOCK_WEBHOOK_SECRET" envDefault:"mock_webhook_secret"`
	BaseURL        string        `env:"PG_MOCK_BASE_URL" envDefault:"http://provider-mock:9101"`
}

func main() {
	app.Run(app.Options{Info: app.Info{Version: version, Commit: commit, BuildDate: buildDate}}, setup)
}

func setup(_ context.Context, rt *app.Runtime, cfg Config) (*app.Hooks, error) {
	svc := providermock.NewService(providermock.Config{
		DefaultLatency: cfg.DefaultLatency, WebhookSecret: cfg.WebhookSecret, BaseURL: cfg.BaseURL,
		Version: rt.Info.Version, Logger: rt.Logger,
	})
	svc.Register(rt.GRPC)
	return &app.Hooks{}, nil
}

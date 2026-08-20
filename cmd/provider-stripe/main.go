// provider-stripe 進入點（Stripe adapter 骨架）。
package main

import (
	"context"

	grpcadapter "github.com/tenghongzou/paymentgateway/internal/providerstripe/adapter/grpc"
	"github.com/tenghongzou/paymentgateway/pkg/app"
	"github.com/tenghongzou/paymentgateway/pkg/config"
)

// 由 -ldflags -X main.version=... 注入。
var version, commit, buildDate = "dev", "none", "unknown"

// Config 為本服務設定。
type Config struct {
	config.Base
	StripeAPIKey        string `env:"PG_STRIPE_API_KEY"`
	StripeWebhookSecret string `env:"PG_STRIPE_WEBHOOK_SECRET"`
	StripeAPIBaseURL    string `env:"PG_STRIPE_API_BASE_URL" envDefault:"https://api.stripe.com"`
}

func main() {
	app.Run(app.Options{Info: app.Info{Version: version, Commit: commit, BuildDate: buildDate}}, setup)
}

func setup(_ context.Context, rt *app.Runtime, _ Config) (*app.Hooks, error) {
	grpcadapter.NewServer(rt.Info.Version).Register(rt.GRPC)
	// TODO(provider-stripe): 建立 Stripe client（cfg.StripeAPIKey / cfg.StripeAPIBaseURL）並實作各 RPC。
	return &app.Hooks{}, nil
}

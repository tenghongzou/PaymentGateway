// api-gateway 進入點：REST :8080（認證、冪等、限流、REST↔gRPC 轉譯、PSP inbound webhook）。
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	merchantv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/merchant/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/gateway"
	"github.com/tenghongzou/paymentgateway/pkg/app"
	"github.com/tenghongzou/paymentgateway/pkg/config"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/idempotency"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
)

// 由 -ldflags -X main.version=... 注入。
var version, commit, buildDate = "dev", "none", "unknown"

// Config 為 api-gateway 設定。
type Config struct {
	config.Base
	// 開發模式憑證：PG_ENV=dev 且三者皆有值時不呼叫 merchant-service。
	DevAPIKey        string `env:"PG_DEV_API_KEY"`
	DevSigningSecret string `env:"PG_DEV_SIGNING_SECRET"`
	DevMerchantID    string `env:"PG_DEV_MERCHANT_ID"`
}

func main() {
	app.Run(app.Options{Info: app.Info{Version: version, Commit: commit, BuildDate: buildDate}, DisableGRPC: true}, setup)
}

func setup(ctx context.Context, rt *app.Runtime, cfg Config) (*app.Hooks, error) {
	hooks := &app.Hooks{}
	log := rt.Logger

	if cfg.RedisAddr == "" {
		return nil, errors.New("PG_REDIS_ADDR is required for api-gateway (idempotency / rate limit)")
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DialTimeout: 3 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
	hooks.Closers = append(hooks.Closers, app.Closer{Name: "redis", Close: func(context.Context) error { return rdb.Close() }})
	hooks.Ready = append(hooks.Ready, app.Check{Name: "redis", Fn: func(ctx context.Context) error { return rdb.Ping(ctx).Err() }})

	if cfg.PaymentServiceAddr == "" {
		return nil, errors.New("PG_PAYMENT_SERVICE_ADDR is required")
	}
	payConn, err := grpcx.Dial(ctx, cfg.PaymentServiceAddr, grpc.WithDefaultCallOptions())
	if err != nil {
		return nil, err
	}
	hooks.Closers = append(hooks.Closers, app.Closer{Name: "payment-conn", Close: func(context.Context) error { return payConn.Close() }})
	hooks.Ready = append(hooks.Ready, app.Check{Name: "payment-service", Fn: func(context.Context) error {
		if st := payConn.GetState(); st == connectivity.TransientFailure || st == connectivity.Shutdown {
			return fmt.Errorf("payment-service connection state %s", st)
		}
		return nil
	}})

	var verifier gateway.KeyVerifier
	if cfg.IsDev() && cfg.DevAPIKey != "" && cfg.DevSigningSecret != "" && cfg.DevMerchantID != "" {
		log.Warn("api-gateway running with PG_DEV_* credentials; merchant-service VerifyApiKey is bypassed", "api_key", logx.MaskSecret(cfg.DevAPIKey), "merchant_id", cfg.DevMerchantID)
		verifier = &gateway.DevVerifier{APIKey: cfg.DevAPIKey, SigningSecret: cfg.DevSigningSecret, MerchantID: cfg.DevMerchantID}
	} else {
		if cfg.MerchantServiceAddr == "" {
			return nil, errors.New("PG_MERCHANT_SERVICE_ADDR is required (or set PG_DEV_* in dev)")
		}
		mConn, err := grpcx.Dial(ctx, cfg.MerchantServiceAddr)
		if err != nil {
			return nil, err
		}
		hooks.Closers = append(hooks.Closers, app.Closer{Name: "merchant-conn", Close: func(context.Context) error { return mConn.Close() }})
		verifier = gateway.NewGRPCVerifier(merchantv1.NewMerchantServiceClient(mConn))
	}

	providers := map[string]providerv1.ProviderAdapterClient{}
	for name, addr := range cfg.ProviderAddrs {
		conn, err := grpcx.Dial(ctx, addr)
		if err != nil {
			return nil, err
		}
		hooks.Closers = append(hooks.Closers, app.Closer{Name: "provider-" + name, Close: func(context.Context) error { return conn.Close() }})
		providers[name] = providerv1.NewProviderAdapterClient(conn)
	}

	gw := gateway.New(gateway.Deps{
		Payments: paymentv1.NewPaymentServiceClient(payConn), Providers: providers, Verifier: verifier,
		Replay: gateway.NewRedisReplayDetector(rdb), Idem: idempotency.NewRedisStore(rdb, cfg.IdempotencyTTL),
		Limiter: gateway.NewRedisRateLimiter(rdb, cfg.RateLimitRPS), RPS: cfg.RateLimitRPS, Logger: log,
	})
	rt.Mux.Handle("/", gw.Handler())
	return hooks, nil
}

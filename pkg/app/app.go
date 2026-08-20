// Package app 提供所有服務共用的啟動骨架（docs/07 §1.8 運維契約）：
//
//	載入設定 → logger → OTel → （可選）auto migrate → HTTP(/healthz /readyz /metrics) → gRPC → 等待 SIGTERM → 優雅關機。
//
// 也處理 `migrate up|down [N]|version` 子命令（os.Args[1] == "migrate"）。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/tenghongzou/paymentgateway/migrations"
	"github.com/tenghongzou/paymentgateway/pkg/config"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/httpx"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
	"github.com/tenghongzou/paymentgateway/pkg/otel"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// Info 為 ldflags 注入的版本資訊（package main 的 version / commit / buildDate）。
type Info struct {
	Version   string
	Commit    string
	BuildDate string
}

// Options 為 Run 的選項。
type Options struct {
	Info Info
	// MigrationService 為 migrations/<service> 的短名（merchant / payment / ledger / webhook / reconciliation）；空表示無 DB。
	MigrationService string
	// DisableGRPC 為 true 時即使設定了 PG_GRPC_ADDR 也不啟 gRPC（api-gateway 用）。
	DisableGRPC bool
}

// Runtime 為 Setup 可使用的共用元件。
type Runtime struct {
	Info   Info
	Base   config.Base
	Logger *slog.Logger
	// GRPC 為已帶 interceptors / health / reflection 的 server；Setup 在此註冊服務（無 gRPC 時為 nil）。
	GRPC   *grpc.Server
	Health *health.Server
	// Mux 已註冊 /healthz /readyz /metrics；Setup 可再掛載其他 handler（api-gateway 掛 "/"）。
	Mux *http.ServeMux
}

// Check 為 readiness 檢查。
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Worker 為長駐 goroutine（consumer、relay、sweeper）；ctx 結束後應在合理時間內返回。
type Worker struct {
	Name string
	Run  func(ctx context.Context) error
}

// Closer 為關機時要釋放的資源（DB pool、gRPC client conn、producer）。
type Closer struct {
	Name  string
	Close func(ctx context.Context) error
}

// Hooks 為 Setup 回傳的掛勾。
type Hooks struct {
	// Ready 的檢查全部通過 /readyz 才回 200。
	Ready []Check
	// Workers 在伺服器啟動後執行；關機時「在排空請求之前」停止（Kafka consumer、排程）。
	Workers []Worker
	// PostDrainWorkers 在排空請求「之後」才停止（outbox relay：讓 in-flight 請求寫入的事件也能送出）。
	PostDrainWorkers []Worker
	// Closers 在所有 worker 停止後依序執行。
	Closers []Closer
}

// SetupFunc 由各服務實作：建立依賴、註冊 gRPC 服務、回傳 Hooks。
type SetupFunc[T any] func(ctx context.Context, rt *Runtime, cfg T) (*Hooks, error)

// baseProvider 由內嵌 config.Base 的設定型別自動滿足。
type baseProvider interface {
	BaseConfig() config.Base
}

// Run 為服務進入點：處理 migrate 子命令或啟動服務，結束時 os.Exit。
func Run[T any](opts Options, setup SetupFunc[T]) {
	os.Exit(Main(opts, setup, os.Args[1:]))
}

// Main 與 Run 相同但回傳 exit code（測試用）。
func Main[T any](opts Options, setup SetupFunc[T], args []string) int {
	cfg, err := config.Load[T]()
	if err != nil {
		slog.Error("load config", "err", err)
		return 2
	}
	bp, ok := any(cfg).(baseProvider)
	if !ok {
		slog.Error("config type must embed config.Base")
		return 2
	}
	base := bp.BaseConfig()
	log := logx.New(base.ServiceName, base.Env, base.LogLevel)
	slog.SetDefault(log)

	if len(args) > 0 && args[0] == "migrate" {
		return runMigrate(log, base, opts.MigrationService, args[1:])
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := serve(ctx, opts, base, cfg, log, setup); err != nil {
		log.Error("service exited with error", "err", err)
		return 1
	}
	return 0
}

//nolint:contextcheck // 關機各階段刻意使用獨立 ctx（signal ctx 已取消），見各行註解
func serve[T any](ctx context.Context, opts Options, base config.Base, cfg T, log *slog.Logger, setup SetupFunc[T]) error {
	log.Info("starting service",
		"version", opts.Info.Version, "commit", opts.Info.Commit, "build_date", opts.Info.BuildDate,
		"http_addr", base.HTTPAddr, "grpc_addr", base.GRPCAddr)

	// ---- OTel ----
	otelShutdown, err := otel.Setup(ctx, base.ServiceName, base.OTelExporterOTLPEndpoint, otel.Options{
		Insecure: base.OTelExporterOTLPInsecure, Version: opts.Info.Version, Env: base.Env, EnablePrometheus: true,
	})
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer func() { //nolint:contextcheck // 關機階段，原 ctx 已取消
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if serr := otelShutdown(sctx); serr != nil {
			log.Warn("otel shutdown", "err", serr)
		}
	}()

	// ---- auto migrate ----
	if base.AutoMigrate && opts.MigrationService != "" {
		if merr := migrateUp(ctx, log, base, opts.MigrationService); merr != nil {
			return merr
		}
	}

	// ---- runtime ----
	rt := &Runtime{Info: opts.Info, Base: base, Logger: log, Mux: http.NewServeMux()}
	if base.GRPCAddr != "" && !opts.DisableGRPC {
		rt.GRPC, rt.Health = grpcx.NewServer(grpcx.ServerOptions{Logger: log, EnableReflection: !base.IsProduction()})
	}
	var ready atomic.Bool
	var shuttingDown atomic.Bool
	hooks := &Hooks{}
	registerOps(rt, opts.Info, &ready, &shuttingDown, func() []Check { return hooks.Ready })

	h, err := setup(ctx, rt, cfg)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if h != nil {
		*hooks = *h
	}
	defer runClosers(log, hooks.Closers) //nolint:contextcheck // 關機階段，原 ctx 已取消

	// ---- listeners ----
	lc := net.ListenConfig{}
	httpLis, err := lc.Listen(ctx, "tcp", base.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http %s: %w", base.HTTPAddr, err)
	}
	var grpcLis net.Listener
	if rt.GRPC != nil {
		grpcLis, err = lc.Listen(ctx, "tcp", base.GRPCAddr)
		if err != nil {
			_ = httpLis.Close()
			return fmt.Errorf("listen grpc %s: %w", base.GRPCAddr, err)
		}
	}

	// ---- run servers + workers ----
	// 伺服器與 worker 的 ctx 刻意不繼承 signal ctx：收到 SIGTERM 後要依序（而非同時）停止各元件。
	serversCtx, stopServers := context.WithCancel(context.Background()) //nolint:contextcheck // 見上
	defer stopServers()
	workersCtx, stopWorkers := context.WithCancel(context.Background()) //nolint:contextcheck // 見上
	defer stopWorkers()
	postDrainCtx, stopPostDrain := context.WithCancel(context.Background()) //nolint:contextcheck // 見上
	defer stopPostDrain()

	errCh := make(chan error, 8)
	var wg sync.WaitGroup

	httpSrv := &http.Server{Handler: rt.Mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := httpSrv.Serve(httpLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http serve: %w", err)
		}
	}()
	if rt.GRPC != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := grpcx.Serve(serversCtx, rt.GRPC, grpcLis, base.ShutdownTimeout); err != nil {
				errCh <- err
			}
		}()
		rt.Health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	}
	var workerWg, postWg sync.WaitGroup
	startWorkers(workersCtx, log, hooks.Workers, &workerWg, errCh)          //nolint:contextcheck // 見 workersCtx 註解
	startWorkers(postDrainCtx, log, hooks.PostDrainWorkers, &postWg, errCh) //nolint:contextcheck // 見 postDrainCtx 註解

	ready.Store(true)
	log.Info("service ready", "http_addr", httpLis.Addr().String(), "grpc_addr", addrString(grpcLis))

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case runErr = <-errCh:
		log.Error("component failed, shutting down", "err", runErr)
	}

	// ---- graceful shutdown（docs/07 §1.8 第 3 點 / docs/05 §13.2）----
	shuttingDown.Store(true)
	ready.Store(false)
	if rt.Health != nil {
		rt.Health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	}
	// 1. 停止 consumer / 排程。
	stopWorkers()
	waitWithTimeout(&workerWg, 10*time.Second, log, "workers")
	// 2. 排空 HTTP / gRPC in-flight 請求。
	drainCtx, cancel := context.WithTimeout(context.Background(), base.ShutdownTimeout) //nolint:contextcheck // 關機階段
	defer cancel()
	stopServers()
	if serr := httpSrv.Shutdown(drainCtx); serr != nil { //nolint:contextcheck // 關機階段
		log.Warn("http shutdown", "err", serr)
	}
	waitWithTimeout(&wg, base.ShutdownTimeout, log, "servers")
	// 3. 停止 outbox relay 等 post-drain worker。
	stopPostDrain()
	waitWithTimeout(&postWg, 10*time.Second, log, "post-drain workers")
	// 4. Closers（DB pool、producer、client conns）由 defer 執行；OTel flush 最後。
	log.Info("service stopped")
	return runErr
}

func startWorkers(ctx context.Context, log *slog.Logger, workers []Worker, wg *sync.WaitGroup, errCh chan<- error) {
	for _, w := range workers {
		wg.Add(1)
		go func(w Worker) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("worker %s panicked: %v", w.Name, r)
				}
			}()
			if err := w.Run(ctx); err != nil && ctx.Err() == nil {
				errCh <- fmt.Errorf("worker %s: %w", w.Name, err)
			}
			log.Debug("worker exited", "worker", w.Name)
		}(w)
	}
}

func runClosers(log *slog.Logger, closers []Closer) {
	for _, c := range closers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) //nolint:contextcheck // 關機階段
		if err := c.Close(ctx); err != nil {
			log.Warn("close failed", "component", c.Name, "err", err)
		}
		cancel()
	}
}

func waitWithTimeout(wg *sync.WaitGroup, d time.Duration, log *slog.Logger, what string) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		log.Warn("timed out waiting", "for", what, "timeout", d)
	}
}

func addrString(l net.Listener) string {
	if l == nil {
		return ""
	}
	return l.Addr().String()
}

// registerOps 註冊 /healthz /readyz /metrics。
func registerOps(rt *Runtime, info Info, ready, shuttingDown *atomic.Bool, checks func() []Check) {
	rt.Mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "service": rt.Base.ServiceName, "version": info.Version, "commit": info.Commit, "build_date": info.BuildDate,
		})
	})
	rt.Mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if shuttingDown.Load() || !ready.Load() {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		results := map[string]string{}
		ok := true
		for _, c := range checks() {
			if err := c.Fn(ctx); err != nil {
				ok = false
				results[c.Name] = err.Error()
			} else {
				results[c.Name] = "ok"
			}
		}
		status := http.StatusOK
		state := "ready"
		if !ok {
			status = http.StatusServiceUnavailable
			state = "not_ready"
		}
		httpx.WriteJSON(w, status, map[string]any{"status": state, "checks": results})
	})
	rt.Mux.Handle("GET /metrics", promhttp.Handler())
}

func migrateUp(ctx context.Context, log *slog.Logger, base config.Base, service string) error {
	src, err := migrations.Source(service)
	if err != nil {
		return err
	}
	url := base.EffectiveMigrateURL()
	if url == "" {
		return errors.New("PG_AUTO_MIGRATE=true but PG_DATABASE_URL is empty")
	}
	start := time.Now()
	if err := pgdb.Migrate(ctx, url, service, src); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	log.Info("migrations applied", "service", service, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

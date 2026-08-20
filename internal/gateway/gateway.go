package gateway

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/httpx"
	"github.com/tenghongzou/paymentgateway/pkg/idempotency"
)

// Deps 為 Gateway 的依賴。
type Deps struct {
	Payments  paymentv1.PaymentServiceClient
	Providers map[string]providerv1.ProviderAdapterClient
	Verifier  KeyVerifier
	Replay    ReplayDetector
	Idem      idempotency.Store
	Limiter   RateLimiter
	RPS       int
	Logger    *slog.Logger
	Clock     func() time.Time
	// RequestTimeout 為每個請求的上限（預設 30s，略大於 payment saga 25s）。
	RequestTimeout time.Duration
}

// Gateway 組裝 middleware 與 handlers。
type Gateway struct {
	payments  paymentv1.PaymentServiceClient
	providers map[string]providerv1.ProviderAdapterClient
	verifier  KeyVerifier
	replay    ReplayDetector
	idem      idempotency.Store
	limiter   RateLimiter
	rps       int
	log       *slog.Logger
	clock     func() time.Time
	timeout   time.Duration
}

// New 建立 Gateway。
func New(d Deps) *Gateway {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Replay == nil {
		d.Replay = NewMemoryReplayDetector()
	}
	if d.RequestTimeout <= 0 {
		d.RequestTimeout = 30 * time.Second
	}
	return &Gateway{payments: d.Payments, providers: d.Providers, verifier: d.Verifier, replay: d.Replay, idem: d.Idem, limiter: d.Limiter, rps: d.RPS, log: d.Logger, clock: d.Clock, timeout: d.RequestTimeout}
}

// Handler 回傳完整的 HTTP handler（/v1/*、/psp/*；/healthz /readyz /metrics 由 pkg/app 提供）。
// middleware 鏈：RequestID → Recover → Logging → otelhttp → Auth → Idempotency → RateLimit。
func (g *Gateway) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recover(g.log), httpx.Logging(g.log))
	r.Use(func(next http.Handler) http.Handler { return otelhttp.NewHandler(next, "api-gateway") })
	r.Use(httpx.Timeout(g.timeout))
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteAppError(w, req, apperr.ErrResourceMissing.WithMessage("no such endpoint: %s %s", req.Method, req.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteAppError(w, req, apperr.ErrParameterInvalid.WithMessage("method %s not allowed", req.Method))
	})

	r.Route("/v1", func(v1 chi.Router) {
		v1.Use(g.Auth, g.RateLimit, g.Idempotency)
		v1.Post("/payments", g.createPayment)
		v1.Get("/payments", g.listPayments)
		v1.Get("/payments/{id}", g.getPayment)
		v1.Post("/payments/{id}/capture", g.capturePayment)
		v1.Post("/payments/{id}/void", g.voidPayment)
		v1.Post("/payments/{id}/confirm", g.confirmPayment)
		v1.Post("/refunds", g.createRefund)
		v1.Get("/refunds/{id}", g.getRefund)
	})
	// PSP inbound webhook：不經商戶認證 / 冪等 / 限流（由 adapter 驗簽）。
	r.Post("/psp/{provider}/webhook", g.providerWebhook)
	return r
}

// Package provider 為 payment-service 對 PSP adapter 的 gRPC client、Registry、Router 與 circuit breaker。
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"

	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/app"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
)

// Client 包裝 providerv1.ProviderAdapterClient 以滿足 app.ProviderClient。
type Client struct {
	name string
	c    providerv1.ProviderAdapterClient
}

// NewClient 以既有的 gRPC client 建立 Client。
func NewClient(name string, c providerv1.ProviderAdapterClient) *Client {
	return &Client{name: name, c: c}
}

// Name 回傳 provider 名稱。
func (c *Client) Name() string { return c.name }

// Authorize 實作 app.ProviderClient。
func (c *Client) Authorize(ctx context.Context, req *providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error) {
	return c.c.Authorize(ctx, req)
}

// Capture 實作 app.ProviderClient。
func (c *Client) Capture(ctx context.Context, req *providerv1.CaptureRequest) (*providerv1.CaptureResponse, error) {
	return c.c.Capture(ctx, req)
}

// Void 實作 app.ProviderClient。
func (c *Client) Void(ctx context.Context, req *providerv1.VoidRequest) (*providerv1.VoidResponse, error) {
	return c.c.Void(ctx, req)
}

// Refund 實作 app.ProviderClient。
func (c *Client) Refund(ctx context.Context, req *providerv1.RefundRequest) (*providerv1.RefundResponse, error) {
	return c.c.Refund(ctx, req)
}

// GetPaymentStatus 實作 app.ProviderClient。
func (c *Client) GetPaymentStatus(ctx context.Context, req *providerv1.GetPaymentStatusRequest) (*providerv1.GetPaymentStatusResponse, error) {
	return c.c.GetPaymentStatus(ctx, req)
}

// HealthCheck 供 Router 取得能力與健康度。
func (c *Client) HealthCheck(ctx context.Context) (*providerv1.HealthCheckResponse, error) {
	return c.c.HealthCheck(ctx, &providerv1.HealthCheckRequest{})
}

// Registry 依 PG_PROVIDER_ADDRS 建立多個 client。
type Registry struct {
	clients map[string]*Client
	conns   []*grpc.ClientConn
}

// NewRegistry 對每個 name=addr 建立連線。
func NewRegistry(ctx context.Context, addrs map[string]string) (*Registry, error) {
	r := &Registry{clients: map[string]*Client{}}
	for name, addr := range addrs {
		conn, err := grpcx.Dial(ctx, addr)
		if err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		r.conns = append(r.conns, conn)
		r.clients[name] = NewClient(name, providerv1.NewProviderAdapterClient(conn))
	}
	return r, nil
}

// NewRegistryFromClients 以既有 client 建立（測試 / bufconn 用）。
func NewRegistryFromClients(clients map[string]providerv1.ProviderAdapterClient) *Registry {
	r := &Registry{clients: map[string]*Client{}}
	for name, c := range clients {
		r.clients[name] = NewClient(name, c)
	}
	return r
}

// Get 實作 app.ProviderRegistry。
func (r *Registry) Get(name string) (app.ProviderClient, bool) {
	c, ok := r.clients[name]
	return c, ok
}

// Names 回傳已設定的 provider 名稱（排序）。
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.clients))
	for n := range r.clients {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Client 取得具體 Client（HealthCheck 用）。
func (r *Registry) Client(name string) (*Client, bool) {
	c, ok := r.clients[name]
	return c, ok
}

// Close 關閉所有連線。
func (r *Registry) Close() error {
	var first error
	for _, c := range r.conns {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---- circuit breaker（docs/02 §9.4 的最小實作：連續 N 次 unavailable 後 open 30s，half-open 放行一筆探測）----

type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// Breaker 為單一 provider 的斷路器。
type Breaker struct {
	mu           sync.Mutex
	state        breakerState
	consecutive  int
	openedAt     time.Time
	probing      bool
	Threshold    int
	OpenDuration time.Duration
	now          func() time.Time
}

// NewBreaker 建立斷路器（threshold 預設 5、open 30s）。
func NewBreaker(threshold int, openFor time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if openFor <= 0 {
		openFor = 30 * time.Second
	}
	return &Breaker{Threshold: threshold, OpenDuration: openFor, now: time.Now}
}

// Allow 回傳是否允許送流量；open 期滿轉 half-open 並允許一筆探測。
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return true
	case stateOpen:
		if b.now().Sub(b.openedAt) >= b.OpenDuration {
			b.state = stateHalfOpen
			b.probing = true
			return true
		}
		return false
	case stateHalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	default:
		return true
	}
}

// Report 回報一次結果；只有 provider 故障類別計入（declined 不算，docs/02 §9.4）。
func (b *Breaker) Report(cat domain.ProviderErrorCategory) {
	b.mu.Lock()
	defer b.mu.Unlock()
	failure := cat == domain.CategoryProviderUnavailable || cat == domain.CategoryProviderTimeout || cat == domain.CategoryProviderConfigError || cat == domain.CategoryProviderRateLimited
	switch {
	case failure:
		b.consecutive++
		if b.state == stateHalfOpen || b.consecutive >= b.Threshold {
			b.state = stateOpen
			b.openedAt = b.now()
			b.probing = false
		}
	default:
		b.consecutive = 0
		b.state = stateClosed
		b.probing = false
	}
}

// State 回傳狀態字串（指標 / log 用）。
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return [...]string{"closed", "open", "half_open"}[b.state]
}

// ---- Router ----

// RouterConfig 為 Router 設定。
type RouterConfig struct {
	// DefaultOrder 為平台預設順序（PG_ROUTING_DEFAULT_ORDER；預設 mock 優先，其餘依名稱）。
	DefaultOrder []string
	// CapabilityTTL 為 HealthCheck 能力快取時間（預設 30s）。
	CapabilityTTL time.Duration
	// BreakerThreshold / BreakerOpen 傳給每個 provider 的 Breaker。
	BreakerThreshold int
	BreakerOpen      time.Duration
	Logger           *slog.Logger
}

// Router 為簡單路由：token_provider 鎖定 → preferred → 預設順序；幣別硬過濾（HealthCheck capabilities）；circuit open 者排除。
type Router struct {
	reg      *Registry
	cfg      RouterConfig
	breakers map[string]*Breaker
	mu       sync.Mutex
	caps     map[string]capEntry
	now      func() time.Time
}

type capEntry struct {
	currencies map[string]bool // 空 map = 全部支援
	serving    bool
	fetchedAt  time.Time
}

// NewRouter 建立 Router。
func NewRouter(reg *Registry, cfg RouterConfig) *Router {
	if cfg.CapabilityTTL <= 0 {
		cfg.CapabilityTTL = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	r := &Router{reg: reg, cfg: cfg, breakers: map[string]*Breaker{}, caps: map[string]capEntry{}, now: time.Now}
	for _, n := range reg.Names() {
		r.breakers[n] = NewBreaker(cfg.BreakerThreshold, cfg.BreakerOpen)
	}
	return r
}

// Route 實作 app.Router。
func (r *Router) Route(ctx context.Context, rc app.RoutingContext) ([]app.Candidate, error) {
	ordered := r.orderedNames()
	var out []app.Candidate
	add := func(name, reason string) {
		for _, c := range out {
			if c.Provider == name {
				return
			}
		}
		if _, ok := r.reg.Get(name); !ok {
			return
		}
		if !r.supportsCurrency(ctx, name, rc.Amount.Currency) {
			return
		}
		if b := r.breakers[name]; b != nil && !b.Allow() {
			r.cfg.Logger.Debug("router: provider circuit open, skipped", "provider", name)
			return
		}
		out = append(out, app.Candidate{Provider: name, Reason: reason})
	}
	// token 不可攜：鎖定在發 token 的 provider。
	if rc.TokenProvider != "" {
		add(rc.TokenProvider, "token_provider")
		return out, nil
	}
	if rc.PreferredProvider != "" {
		add(rc.PreferredProvider, "preferred")
	}
	for _, n := range ordered {
		add(n, "default")
	}
	return out, nil
}

// Report 實作 app.Router。
func (r *Router) Report(provider string, cat domain.ProviderErrorCategory) {
	if b := r.breakers[provider]; b != nil {
		b.Report(cat)
	}
}

// BreakerState 回傳某 provider 的斷路器狀態（測試 / 指標）。
func (r *Router) BreakerState(provider string) string {
	if b := r.breakers[provider]; b != nil {
		return b.State()
	}
	return ""
}

func (r *Router) orderedNames() []string {
	names := r.reg.Names()
	order := r.cfg.DefaultOrder
	if len(order) == 0 {
		order = []string{"mock"}
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range order {
		for _, have := range names {
			if have == n && !seen[n] {
				out = append(out, n)
				seen[n] = true
			}
		}
	}
	for _, n := range names {
		if !seen[n] {
			out = append(out, n)
		}
	}
	return out
}

// supportsCurrency 以 HealthCheck capabilities（快取）做幣別硬過濾；HealthCheck 失敗時保守視為支援（讓 Authorize 自行判定）。
func (r *Router) supportsCurrency(ctx context.Context, name, currency string) bool {
	r.mu.Lock()
	entry, ok := r.caps[name]
	fresh := ok && r.now().Sub(entry.fetchedAt) < r.cfg.CapabilityTTL
	r.mu.Unlock()
	if !fresh {
		entry = r.fetchCaps(ctx, name)
	}
	if !entry.serving {
		return false
	}
	if len(entry.currencies) == 0 {
		return true
	}
	return entry.currencies[currency]
}

func (r *Router) fetchCaps(ctx context.Context, name string) capEntry {
	entry := capEntry{serving: true, fetchedAt: r.now()}
	c, ok := r.reg.Client(name)
	if ok {
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		resp, err := c.HealthCheck(hctx)
		cancel()
		if err == nil {
			entry.serving = resp.GetStatus() != providerv1.HealthStatus_HEALTH_STATUS_NOT_SERVING
			if cur := resp.GetCapabilities().GetCurrencies(); len(cur) > 0 {
				entry.currencies = map[string]bool{}
				for _, cc := range cur {
					entry.currencies[cc] = true
				}
			}
		}
	}
	r.mu.Lock()
	r.caps[name] = entry
	r.mu.Unlock()
	return entry
}

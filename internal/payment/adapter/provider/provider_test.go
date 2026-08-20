package provider

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/app"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// stubAdapter 只實作 HealthCheck（其餘回 nil）。
type stubAdapter struct {
	providerv1.ProviderAdapterClient
	currencies []string
	status     providerv1.HealthStatus
	calls      int
}

func (s *stubAdapter) HealthCheck(context.Context, *providerv1.HealthCheckRequest, ...grpc.CallOption) (*providerv1.HealthCheckResponse, error) {
	s.calls++
	return &providerv1.HealthCheckResponse{Status: s.status, Capabilities: &providerv1.ProviderCapabilities{Currencies: s.currencies}}, nil
}

func TestBreaker(t *testing.T) {
	now := time.Unix(1000, 0)
	b := NewBreaker(3, 30*time.Second)
	b.now = func() time.Time { return now }
	assert.True(t, b.Allow())
	b.Report(domain.CategoryDeclinedHard) // declined 不計入
	b.Report(domain.CategoryProviderUnavailable)
	b.Report(domain.CategoryProviderUnavailable)
	assert.True(t, b.Allow())
	assert.Equal(t, "closed", b.State())
	b.Report(domain.CategoryProviderTimeout)
	assert.Equal(t, "open", b.State())
	assert.False(t, b.Allow())
	now = now.Add(31 * time.Second)
	assert.True(t, b.Allow(), "half-open probe allowed")
	assert.Equal(t, "half_open", b.State())
	assert.False(t, b.Allow(), "only one probe at a time")
	b.Report(domain.CategoryProviderUnavailable)
	assert.Equal(t, "open", b.State())
	now = now.Add(31 * time.Second)
	assert.True(t, b.Allow())
	b.Report(domain.CategoryNone)
	assert.Equal(t, "closed", b.State())
	assert.True(t, b.Allow())
}

func TestRouterOrderingAndFilters(t *testing.T) {
	mock := &stubAdapter{currencies: []string{"TWD", "USD"}, status: providerv1.HealthStatus_HEALTH_STATUS_SERVING}
	stripe := &stubAdapter{currencies: []string{"USD"}, status: providerv1.HealthStatus_HEALTH_STATUS_SERVING}
	down := &stubAdapter{status: providerv1.HealthStatus_HEALTH_STATUS_NOT_SERVING}
	reg := NewRegistryFromClients(map[string]providerv1.ProviderAdapterClient{"mock": mock, "stripe": stripe, "down": down})
	assert.Equal(t, []string{"down", "mock", "stripe"}, reg.Names())
	r := NewRouter(reg, RouterConfig{BreakerThreshold: 2})
	ctx := context.Background()

	// 預設順序 mock → 其餘；幣別過濾：TWD 只有 mock。
	c, err := r.Route(ctx, app.RoutingContext{Amount: money.MustNew(100, "TWD")})
	require.NoError(t, err)
	require.Len(t, c, 1)
	assert.Equal(t, "mock", c[0].Provider)
	assert.Equal(t, "default", c[0].Reason)

	c = must(r.Route(ctx, app.RoutingContext{Amount: money.MustNew(100, "USD")}))
	require.Len(t, c, 2)
	assert.Equal(t, []string{"mock", "stripe"}, []string{c[0].Provider, c[1].Provider})

	// preferred 置頂。
	c = must(r.Route(ctx, app.RoutingContext{Amount: money.MustNew(100, "USD"), PreferredProvider: "stripe"}))
	assert.Equal(t, "stripe", c[0].Provider)
	assert.Equal(t, "preferred", c[0].Reason)

	// token_provider 鎖定。
	c = must(r.Route(ctx, app.RoutingContext{Amount: money.MustNew(100, "USD"), TokenProvider: "stripe"}))
	require.Len(t, c, 1)
	assert.Equal(t, "token_provider", c[0].Reason)
	c = must(r.Route(ctx, app.RoutingContext{Amount: money.MustNew(100, "TWD"), TokenProvider: "stripe"}))
	assert.Empty(t, c, "token provider does not support currency")
	c = must(r.Route(ctx, app.RoutingContext{Amount: money.MustNew(100, "USD"), TokenProvider: "ghost"}))
	assert.Empty(t, c)

	// 能力快取：多次 Route 只呼叫一次 HealthCheck。
	assert.Equal(t, 1, mock.calls)

	// circuit open 後排除。
	r.Report("mock", domain.CategoryProviderUnavailable)
	r.Report("mock", domain.CategoryProviderUnavailable)
	assert.Equal(t, "open", r.BreakerState("mock"))
	c = must(r.Route(ctx, app.RoutingContext{Amount: money.MustNew(100, "USD")}))
	require.Len(t, c, 1)
	assert.Equal(t, "stripe", c[0].Provider)
	assert.Empty(t, r.BreakerState("nope"))
	_, ok := reg.Get("mock")
	assert.True(t, ok)
	require.NoError(t, reg.Close())
}

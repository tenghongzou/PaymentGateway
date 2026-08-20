package app

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
)

// DefaultEndpointCacheTTL 為端點快取存活時間。
const DefaultEndpointCacheTTL = 60 * time.Second

// EndpointCache 以商戶為單位快取 EndpointSource 的結果（記憶體、TTL），
// 上游暫時失敗時回傳過期資料（最多 staleFor），避免 merchant-service 抖動造成投遞停擺。
type EndpointCache struct {
	src      EndpointSource
	ttl      time.Duration
	staleFor time.Duration
	clock    Clock

	mu      sync.Mutex
	entries map[uuid.UUID]*cacheEntry
	// inflight 讓同一商戶的並發 miss 只打一次上游（single-flight）。
	inflight map[uuid.UUID]*cacheCall
}

type cacheEntry struct {
	endpoints []*domain.Endpoint
	fetchedAt time.Time
}

type cacheCall struct {
	done chan struct{}
	eps  []*domain.Endpoint
	err  error
}

// NewEndpointCache 建立快取；ttl <= 0 時使用 DefaultEndpointCacheTTL。
func NewEndpointCache(src EndpointSource, ttl time.Duration, clock Clock) *EndpointCache {
	if ttl <= 0 {
		ttl = DefaultEndpointCacheTTL
	}
	if clock == nil {
		clock = ClockFunc(time.Now)
	}
	return &EndpointCache{
		src: src, ttl: ttl, staleFor: 10 * ttl, clock: clock,
		entries: map[uuid.UUID]*cacheEntry{}, inflight: map[uuid.UUID]*cacheCall{},
	}
}

// ListEndpoints 實作 EndpointSource。
func (c *EndpointCache) ListEndpoints(ctx context.Context, merchantID uuid.UUID) ([]*domain.Endpoint, error) {
	now := c.clock.Now()
	c.mu.Lock()
	if e, ok := c.entries[merchantID]; ok && now.Sub(e.fetchedAt) < c.ttl {
		c.mu.Unlock()
		return e.endpoints, nil
	}
	if call, ok := c.inflight[merchantID]; ok {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.eps, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &cacheCall{done: make(chan struct{})}
	c.inflight[merchantID] = call
	c.mu.Unlock()

	eps, err := c.src.ListEndpoints(ctx, merchantID)

	c.mu.Lock()
	delete(c.inflight, merchantID)
	if err == nil {
		c.entries[merchantID] = &cacheEntry{endpoints: eps, fetchedAt: c.clock.Now()}
	} else if e, ok := c.entries[merchantID]; ok && now.Sub(e.fetchedAt) < c.staleFor {
		// 上游失敗：回過期資料。
		eps, err = e.endpoints, nil
	}
	c.mu.Unlock()
	call.eps, call.err = eps, err
	close(call.done)
	return eps, err
}

// GetEndpoint 實作 EndpointSource（從快取的清單中找）。
func (c *EndpointCache) GetEndpoint(ctx context.Context, merchantID, endpointID uuid.UUID) (*domain.Endpoint, error) {
	eps, err := c.ListEndpoints(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	for _, ep := range eps {
		if ep.ID == endpointID {
			return ep, nil
		}
	}
	return nil, nil
}

// Invalidate 讓商戶的快取立即失效（端點被停用 / 輪替時呼叫）。
func (c *EndpointCache) Invalidate(merchantID uuid.UUID) {
	c.mu.Lock()
	delete(c.entries, merchantID)
	c.mu.Unlock()
}

// Len 回傳目前快取的商戶數（測試 / 指標用）。
func (c *EndpointCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

var _ EndpointSource = (*EndpointCache)(nil)
var _ EndpointInvalidator = (*EndpointCache)(nil)

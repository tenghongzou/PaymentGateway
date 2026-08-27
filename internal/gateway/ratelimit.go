package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/httpx"
)

// RateLimiter 為每商戶限流介面。
type RateLimiter interface {
	// Allow 回傳是否放行；拒絕時附 Retry-After 秒數。
	Allow(ctx context.Context, merchantID string) (allowed bool, retryAfter int, err error)
}

// ValkeyRateLimiter 為固定視窗（每秒）限流：INCR rl:{merchant}:{sec} + EXPIRE 2。
type ValkeyRateLimiter struct {
	vdb valkeycompat.Cmdable
	rps int
	now func() time.Time
}

// NewValkeyRateLimiter 建立 Valkey 限流器。
func NewValkeyRateLimiter(vdb valkeycompat.Cmdable, rps int) *ValkeyRateLimiter {
	return &ValkeyRateLimiter{vdb: vdb, rps: rps, now: time.Now}
}

// Allow 實作 RateLimiter。
func (l *ValkeyRateLimiter) Allow(ctx context.Context, merchantID string) (bool, int, error) {
	if l.rps <= 0 {
		return true, 0, nil
	}
	key := fmt.Sprintf("rl:%s:%d", merchantID, l.now().Unix())
	pipe := l.vdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 2*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, 0, err
	}
	return incr.Val() <= int64(l.rps), 1, nil
}

// MemoryRateLimiter 為記憶體固定視窗實作（測試 / 單機）。
type MemoryRateLimiter struct {
	mu     sync.Mutex
	rps    int
	counts map[string]int
	window int64
	now    func() time.Time
}

// NewMemoryRateLimiter 建立記憶體限流器。
func NewMemoryRateLimiter(rps int) *MemoryRateLimiter {
	return &MemoryRateLimiter{rps: rps, counts: map[string]int{}, now: time.Now}
}

// Allow 實作 RateLimiter。
func (l *MemoryRateLimiter) Allow(_ context.Context, merchantID string) (bool, int, error) {
	if l.rps <= 0 {
		return true, 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	sec := l.now().Unix()
	if sec != l.window {
		l.window = sec
		l.counts = map[string]int{}
	}
	l.counts[merchantID]++
	return l.counts[merchantID] <= l.rps, 1, nil
}

// RateLimit middleware：超過每商戶 RPS 回 429（Valkey 失敗時 fail-open 並記 log）。
func (g *Gateway) RateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := PrincipalFromContext(r.Context())
		if principal == nil || g.limiter == nil {
			next.ServeHTTP(w, r)
			return
		}
		allowed, retryAfter, err := g.limiter.Allow(r.Context(), principal.MerchantID)
		if err != nil {
			g.log.Warn("rate limiter unavailable; allowing request", "err", err)
			next.ServeHTTP(w, r)
			return
		}
		if !allowed {
			w.Header().Set(httpx.HeaderRetryAfter, strconv.Itoa(retryAfter))
			httpx.WriteAppError(w, r, apperr.ErrRateLimited.WithMessage("Rate limit of %d requests per second exceeded.", g.rps))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ValkeyReplayDetector 以 SET replay:{key_id}:{sig[:32]} NX EX 300 偵測重放（docs/06 §3.3 第 8 點）。
type ValkeyReplayDetector struct {
	vdb valkeycompat.Cmdable
}

// NewValkeyReplayDetector 建立 Valkey 實作。
func NewValkeyReplayDetector(vdb valkeycompat.Cmdable) *ValkeyReplayDetector {
	return &ValkeyReplayDetector{vdb: vdb}
}

// Seen 實作 ReplayDetector。
func (d *ValkeyReplayDetector) Seen(ctx context.Context, keyID, frag string) (bool, error) {
	ok, err := d.vdb.SetNX(ctx, "replay:"+keyID+":"+frag, 1, 300*time.Second).Result()
	if err != nil {
		return false, err
	}
	return !ok, nil
}

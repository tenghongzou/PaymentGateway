package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"
)

// ValkeyStore 為 Valkey 實作（SETNX 鎖 + JSON 值）。
type ValkeyStore struct {
	vdb     valkeycompat.Cmdable
	ttl     time.Duration
	lockTTL time.Duration
}

// NewValkeyStore 建立 Valkey 儲存；ttl ≤ 0 時用 DefaultTTL。
func NewValkeyStore(vdb valkeycompat.Cmdable, ttl time.Duration) *ValkeyStore {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &ValkeyStore{vdb: vdb, ttl: ttl, lockTTL: DefaultLockTTL}
}

// Begin 實作 Store。
func (s *ValkeyStore) Begin(ctx context.Context, merchantID, key, requestHash string) (State, *Response, error) {
	k := storeKey(merchantID, key)
	rec := record{State: stateInProgress, RequestHash: requestHash, CreatedAt: time.Now().UTC()}
	raw, err := json.Marshal(rec)
	if err != nil {
		return StateNew, nil, fmt.Errorf("idempotency: marshal: %w", err)
	}
	ok, err := s.vdb.SetNX(ctx, k, raw, s.lockTTL).Result()
	if err != nil {
		return StateNew, nil, fmt.Errorf("idempotency: setnx: %w", err)
	}
	if ok {
		return StateNew, nil, nil
	}
	cur, err := s.vdb.Get(ctx, k).Bytes()
	if err != nil {
		if errors.Is(err, valkeycompat.Nil) {
			// 鎖在 SETNX 與 GET 之間過期：視為處理中，讓商戶稍後重試。
			return StateNew, nil, ErrInProgress
		}
		return StateNew, nil, fmt.Errorf("idempotency: get: %w", err)
	}
	var existing record
	if err := json.Unmarshal(cur, &existing); err != nil {
		return StateNew, nil, fmt.Errorf("idempotency: unmarshal: %w", err)
	}
	if existing.RequestHash != requestHash {
		return StateNew, nil, ErrMismatch
	}
	if existing.State == stateInProgress {
		return StateNew, nil, ErrInProgress
	}
	return StateCompleted, existing.Response, nil
}

// Complete 實作 Store。
func (s *ValkeyStore) Complete(ctx context.Context, merchantID, key string, resp Response) error {
	k := storeKey(merchantID, key)
	cur, err := s.vdb.Get(ctx, k).Bytes()
	var rec record
	switch {
	case err == nil:
		if uerr := json.Unmarshal(cur, &rec); uerr != nil {
			rec = record{}
		}
	case errors.Is(err, valkeycompat.Nil):
	default:
		return fmt.Errorf("idempotency: get: %w", err)
	}
	rec.State = stateCompleted
	rec.Response = &resp
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("idempotency: marshal: %w", err)
	}
	if err := s.vdb.Set(ctx, k, raw, s.ttl).Err(); err != nil {
		return fmt.Errorf("idempotency: set: %w", err)
	}
	return nil
}

// Abort 實作 Store。
func (s *ValkeyStore) Abort(ctx context.Context, merchantID, key string) error {
	if err := s.vdb.Del(ctx, storeKey(merchantID, key)).Err(); err != nil {
		return fmt.Errorf("idempotency: del: %w", err)
	}
	return nil
}

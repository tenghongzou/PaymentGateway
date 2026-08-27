package idempotency

import (
	"context"
	"sync"
	"time"
)

// MemoryStore 為記憶體實作（測試 / 單機開發用）。
type MemoryStore struct {
	mu      sync.Mutex
	items   map[string]memItem
	ttl     time.Duration
	lockTTL time.Duration
	now     func() time.Time
}

type memItem struct {
	rec       record
	expiresAt time.Time
}

// NewMemoryStore 建立記憶體儲存。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: map[string]memItem{}, ttl: DefaultTTL, lockTTL: DefaultLockTTL, now: time.Now}
}

// WithClock 設定時間來源（測試用）。
func (m *MemoryStore) WithClock(now func() time.Time) *MemoryStore {
	m.now = now
	return m
}

// Begin 實作 Store。
func (m *MemoryStore) Begin(_ context.Context, merchantID, key, requestHash string) (State, *Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := storeKey(merchantID, key)
	now := m.now()
	if it, ok := m.items[k]; ok && now.Before(it.expiresAt) {
		if it.rec.RequestHash != requestHash {
			return StateNew, nil, ErrMismatch
		}
		if it.rec.State == stateInProgress {
			return StateNew, nil, ErrInProgress
		}
		return StateCompleted, it.rec.Response, nil
	}
	m.items[k] = memItem{
		rec:       record{State: stateInProgress, RequestHash: requestHash, CreatedAt: now},
		expiresAt: now.Add(m.lockTTL),
	}
	return StateNew, nil, nil
}

// Complete 實作 Store。
func (m *MemoryStore) Complete(_ context.Context, merchantID, key string, resp Response) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := storeKey(merchantID, key)
	now := m.now()
	it := m.items[k]
	it.rec.State = stateCompleted
	it.rec.Response = &resp
	if it.rec.CreatedAt.IsZero() {
		it.rec.CreatedAt = now
	}
	it.expiresAt = now.Add(m.ttl)
	m.items[k] = it
	return nil
}

// Abort 實作 Store。
func (m *MemoryStore) Abort(_ context.Context, merchantID, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, storeKey(merchantID, key))
	return nil
}

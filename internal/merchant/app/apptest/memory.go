// Package apptest 提供 app ports 的記憶體實作（fake），供 app / adapter/grpc 單元測試使用。
package apptest

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/merchant/app"
	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// Clock 為可撥動的固定時鐘。
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock 建立固定時鐘。
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now 回傳目前時間。
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 往前撥動。
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Memory 為所有 repo 的共用記憶體儲存。
type Memory struct {
	mu        sync.Mutex
	merchants map[uuid.UUID]*domain.Merchant
	keys      map[uuid.UUID]*domain.ApiKey
	hooks     map[uuid.UUID]*domain.WebhookEndpoint
	routing   map[uuid.UUID]*domain.RoutingPreferences
	outbox    []app.OutboxMessage
	// Clock 供 List 等需要「現在」的查詢使用；nil 時用 time.Now。
	Clock app.Clock
	// FailNext 非 nil 時，下一次任何 repo 寫入回傳此錯誤（測試 rollback 用）。
	FailNext error
	// TxDepth 記錄 WithinTx 巢狀深度（測試用）。
	TxCalls int
}

// NewMemory 建立空儲存。
func NewMemory() *Memory {
	return &Memory{
		merchants: map[uuid.UUID]*domain.Merchant{},
		keys:      map[uuid.UUID]*domain.ApiKey{},
		hooks:     map[uuid.UUID]*domain.WebhookEndpoint{},
		routing:   map[uuid.UUID]*domain.RoutingPreferences{},
	}
}

// Deps 組出 app.Deps（clock / cipher / logger 由呼叫端補）。
func (m *Memory) Deps() app.Deps {
	return app.Deps{
		Tx: txManager{m}, Merchants: merchantRepo{m}, APIKeys: keyRepo{m}, Webhooks: hookRepo{m}, Routing: routingRepo{m}, Outbox: outboxStore{m},
	}
}

// Outbox 回傳所有已寫入的 outbox 訊息（複本）。
func (m *Memory) Outbox() []app.OutboxMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.outbox)
}

// OutboxTypes 回傳事件型別序列。
func (m *Memory) OutboxTypes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.outbox))
	for _, e := range m.outbox {
		out = append(out, e.EventType)
	}
	return out
}

// ResetOutbox 清空 outbox。
func (m *Memory) ResetOutbox() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outbox = nil
}

// Key 直接取得 key（測試檢查 last_used_at 等）。
func (m *Memory) Key(id uuid.UUID) *domain.ApiKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[id]; ok {
		c := *k
		return &c
	}
	return nil
}

// PutKey 直接寫入 key（測試組合特殊狀態用）。
func (m *Memory) PutKey(k *domain.ApiKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *k
	m.keys[k.ID] = &c
}

func (m *Memory) now() time.Time {
	if m.Clock != nil {
		return m.Clock.Now()
	}
	return time.Now()
}

func (m *Memory) failNext() error {
	if m.FailNext != nil {
		err := m.FailNext
		m.FailNext = nil
		return err
	}
	return nil
}

// ---- TxManager ----

type txManager struct{ m *Memory }

// WithinTx 直接執行 fn（記憶體實作無真正交易）。
func (t txManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	t.m.mu.Lock()
	t.m.TxCalls++
	t.m.mu.Unlock()
	// 記憶體實作沒有 rollback；測試以 FailNext 驗證「錯誤會往上傳」即可。
	return fn(ctx)
}

// ---- MerchantRepo ----

type merchantRepo struct{ m *Memory }

func copyMerchant(m *domain.Merchant) *domain.Merchant {
	c := *m
	c.Metadata = maps(m.Metadata)
	c.Settings.FallbackProviders = slices.Clone(m.Settings.FallbackProviders)
	return &c
}

func maps(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Create 寫入商戶複本。
func (r merchantRepo) Create(_ context.Context, m *domain.Merchant) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if err := r.m.failNext(); err != nil {
		return err
	}
	r.m.merchants[m.ID] = copyMerchant(m)
	return nil
}

// Get 依 id 取得商戶複本。
func (r merchantRepo) Get(_ context.Context, id uuid.UUID) (*domain.Merchant, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	m, ok := r.m.merchants[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyMerchant(m), nil
}

// GetForUpdate 等同 Get（記憶體實作無列鎖）。
func (r merchantRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*domain.Merchant, error) {
	return r.Get(ctx, id)
}

// FindByExternalRef 依 external_ref 尋找商戶。
func (r merchantRepo) FindByExternalRef(_ context.Context, ref string) (*domain.Merchant, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	for _, m := range r.m.merchants {
		if m.Settings.ExternalRef == ref {
			return copyMerchant(m), nil
		}
	}
	return nil, domain.ErrNotFound
}

// Update 以樂觀鎖（Version）更新商戶。
func (r merchantRepo) Update(_ context.Context, m *domain.Merchant) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if err := r.m.failNext(); err != nil {
		return err
	}
	cur, ok := r.m.merchants[m.ID]
	if !ok {
		return domain.ErrNotFound
	}
	if cur.Version != m.Version {
		return domain.ErrConcurrentModify
	}
	m.Version++
	r.m.merchants[m.ID] = copyMerchant(m)
	return nil
}

// List 依條件過濾並以 created_at 排序（記憶體實作不分頁）。
func (r merchantRepo) List(_ context.Context, f app.MerchantFilter, p app.Page) ([]*domain.Merchant, string, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	var out []*domain.Merchant
	for _, m := range r.m.merchants {
		if f.Status != "" && m.Status != f.Status {
			continue
		}
		if f.Country != "" && m.Country != f.Country {
			continue
		}
		out = append(out, copyMerchant(m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > p.Size {
		out = out[:p.Size]
	}
	return out, "", nil
}

// ---- ApiKeyRepo ----

type keyRepo struct{ m *Memory }

func copyKey(k *domain.ApiKey) *domain.ApiKey {
	c := *k
	c.Scopes = slices.Clone(k.Scopes)
	return &c
}

// Create 寫入 key 複本；prefix 重複回 ErrAlreadyExists。
func (r keyRepo) Create(_ context.Context, k *domain.ApiKey) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if err := r.m.failNext(); err != nil {
		return err
	}
	for _, existing := range r.m.keys {
		if existing.Prefix == k.Prefix {
			return domain.ErrAlreadyExists
		}
	}
	r.m.keys[k.ID] = copyKey(k)
	return nil
}

// Get 依商戶 + id 取得 key 複本。
func (r keyRepo) Get(_ context.Context, merchantID, id uuid.UUID) (*domain.ApiKey, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	k, ok := r.m.keys[id]
	if !ok || k.MerchantID != merchantID {
		return nil, domain.ErrNotFound
	}
	return copyKey(k), nil
}

// FindByPrefix 依 prefix 找出候選 key。
func (r keyRepo) FindByPrefix(_ context.Context, prefix string) ([]*domain.ApiKey, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	var out []*domain.ApiKey
	for _, k := range r.m.keys {
		if k.Prefix == prefix {
			out = append(out, copyKey(k))
		}
	}
	return out, nil
}

// Update 覆寫既有 key。
func (r keyRepo) Update(_ context.Context, k *domain.ApiKey) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if err := r.m.failNext(); err != nil {
		return err
	}
	if _, ok := r.m.keys[k.ID]; !ok {
		return domain.ErrNotFound
	}
	r.m.keys[k.ID] = copyKey(k)
	return nil
}

// CountActive 計算指定 mode 的有效 key 數。
func (r keyRepo) CountActive(_ context.Context, merchantID uuid.UUID, mode domain.Mode, now time.Time) (int, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	n := 0
	for _, k := range r.m.keys {
		if k.MerchantID == merchantID && k.Mode == mode && k.Status(now) == domain.KeyActive {
			n++
		}
	}
	return n, nil
}

// List 依條件過濾並以 created_at 排序（記憶體實作不分頁）。
func (r keyRepo) List(_ context.Context, merchantID uuid.UUID, f app.ApiKeyFilter, p app.Page) ([]*domain.ApiKey, string, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	now := r.m.now()
	var out []*domain.ApiKey
	for _, k := range r.m.keys {
		if k.MerchantID != merchantID {
			continue
		}
		if f.Mode != "" && k.Mode != f.Mode {
			continue
		}
		if !f.IncludeInactive && k.Status(now) != domain.KeyActive {
			continue
		}
		out = append(out, copyKey(k))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > p.Size {
		out = out[:p.Size]
	}
	return out, "", nil
}

// TouchLastUsed 更新 last_used_at。
func (r keyRepo) TouchLastUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if k, ok := r.m.keys[id]; ok {
		t := at
		k.LastUsedAt = &t
	}
	return nil
}

// ---- WebhookEndpointRepo ----

type hookRepo struct{ m *Memory }

func copyHook(e *domain.WebhookEndpoint) *domain.WebhookEndpoint {
	c := *e
	c.EnabledEvents = slices.Clone(e.EnabledEvents)
	c.Metadata = maps(e.Metadata)
	return &c
}

// Create 寫入端點複本。
func (r hookRepo) Create(_ context.Context, e *domain.WebhookEndpoint) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if err := r.m.failNext(); err != nil {
		return err
	}
	r.m.hooks[e.ID] = copyHook(e)
	return nil
}

// Get 依商戶 + id 取得端點複本。
func (r hookRepo) Get(_ context.Context, merchantID, id uuid.UUID) (*domain.WebhookEndpoint, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	e, ok := r.m.hooks[id]
	if !ok || e.MerchantID != merchantID {
		return nil, domain.ErrNotFound
	}
	return copyHook(e), nil
}

// Update 以樂觀鎖（Version）更新端點。
func (r hookRepo) Update(_ context.Context, e *domain.WebhookEndpoint) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if err := r.m.failNext(); err != nil {
		return err
	}
	cur, ok := r.m.hooks[e.ID]
	if !ok {
		return domain.ErrNotFound
	}
	if cur.Version != e.Version {
		return domain.ErrConcurrentModify
	}
	e.Version++
	r.m.hooks[e.ID] = copyHook(e)
	return nil
}

// CountLive 計算未刪除的端點數。
func (r hookRepo) CountLive(_ context.Context, merchantID uuid.UUID) (int, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	n := 0
	for _, e := range r.m.hooks {
		if e.MerchantID == merchantID && !e.IsDeleted() {
			n++
		}
	}
	return n, nil
}

// List 依條件過濾並以 created_at 排序（記憶體實作不分頁）。
func (r hookRepo) List(_ context.Context, merchantID uuid.UUID, f app.WebhookEndpointFilter, p app.Page) ([]*domain.WebhookEndpoint, string, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	var out []*domain.WebhookEndpoint
	for _, e := range r.m.hooks {
		if e.MerchantID != merchantID {
			continue
		}
		if f.Mode != "" && e.Mode != f.Mode {
			continue
		}
		if !f.IncludeDeleted && e.IsDeleted() {
			continue
		}
		out = append(out, copyHook(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > p.Size {
		out = out[:p.Size]
	}
	return out, "", nil
}

// ---- RoutingPrefRepo ----

type routingRepo struct{ m *Memory }

// Get 取得路由偏好複本。
func (r routingRepo) Get(_ context.Context, merchantID uuid.UUID) (*domain.RoutingPreferences, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	p, ok := r.m.routing[merchantID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	c := *p
	c.Rules = slices.Clone(p.Rules)
	return &c, nil
}

// Upsert 寫入路由偏好並遞增 Version。
func (r routingRepo) Upsert(_ context.Context, p *domain.RoutingPreferences) error {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if err := r.m.failNext(); err != nil {
		return err
	}
	version := 0
	if cur, ok := r.m.routing[p.MerchantID]; ok {
		version = cur.Version + 1
	}
	now := r.m.now()
	p.Version = version
	p.UpdatedAt = &now
	c := *p
	c.Rules = slices.Clone(p.Rules)
	r.m.routing[p.MerchantID] = &c
	return nil
}

// ---- OutboxStore ----

type outboxStore struct{ m *Memory }

// Insert 附加 outbox 訊息；ID 為空時自動產生。
func (o outboxStore) Insert(_ context.Context, msg app.OutboxMessage) (string, error) {
	o.m.mu.Lock()
	defer o.m.mu.Unlock()
	if err := o.m.failNext(); err != nil {
		return "", err
	}
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}
	o.m.outbox = append(o.m.outbox, msg)
	return msg.ID, nil
}

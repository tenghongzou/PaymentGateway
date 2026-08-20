// Package apptest 提供 app ports 的記憶體實作（fake），供 app / adapter 單元測試使用。
package apptest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
)

type MemStore struct {
	mu         sync.Mutex
	processed  map[string]bool
	events     map[uuid.UUID]*domain.Event
	deliveries map[uuid.UUID]*domain.Delivery
	attempts   map[uuid.UUID][]*domain.Attempt
	FailSave   error
}

func NewMemStore() *MemStore {
	return &MemStore{
		processed: map[string]bool{}, events: map[uuid.UUID]*domain.Event{},
		deliveries: map[uuid.UUID]*domain.Delivery{}, attempts: map[uuid.UUID][]*domain.Attempt{},
	}
}

func (m *MemStore) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

func (m *MemStore) MarkProcessed(_ context.Context, eventID uuid.UUID, consumer string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := eventID.String() + "/" + consumer
	if m.processed[k] {
		return true, nil
	}
	m.processed[k] = true
	return false, nil
}

// MemEventRepo 實作 app.EventRepo（與 DeliveryRepo 的 Get 同名，故分開型別）。
type MemEventRepo struct{ *MemStore }

func (m *MemEventRepo) Insert(_ context.Context, ev *domain.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.events[ev.ID]; !ok {
		m.events[ev.ID] = ev
	}
	return nil
}

func (m *MemEventRepo) Get(_ context.Context, merchantID, eventID uuid.UUID) (*domain.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, ok := m.events[eventID]
	if !ok || ev.MerchantID != merchantID {
		return nil, domain.ErrEventNotFound
	}
	return ev, nil
}

func (m *MemStore) InsertPending(_ context.Context, ds []*domain.Delivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range ds {
		dup := false
		for _, x := range m.deliveries {
			if x.EventID == d.EventID && x.EndpointID == d.EndpointID {
				dup = true
			}
		}
		if !dup {
			c := *d
			m.deliveries[d.ID] = &c
		}
	}
	return nil
}

func (m *MemStore) ClaimDue(_ context.Context, now time.Time, limit int) ([]*domain.Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []*domain.Delivery
	for _, d := range m.deliveries {
		if d.IsDue(now) {
			due = append(due, d)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextAttemptAt.Before(due[j].NextAttemptAt) })
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]*domain.Delivery, 0, len(due))
	for _, d := range due {
		if err := d.Claim(now); err != nil {
			return nil, err
		}
		if ev, ok := m.events[d.EventID]; ok {
			d.EventType, d.EventPayload, d.Livemode, d.OccurredAt = ev.Type, ev.Payload, ev.Livemode, ev.OccurredAt
		}
		c := *d
		out = append(out, &c)
	}
	return out, nil
}

func (m *MemStore) Save(_ context.Context, d *domain.Delivery, att *domain.Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailSave != nil {
		return m.FailSave
	}
	cur, ok := m.deliveries[d.ID]
	if !ok {
		return domain.ErrDeliveryNotFound
	}
	if cur.Version != d.Version-1 {
		return errors.New("concurrent modification")
	}
	c := *d
	m.deliveries[d.ID] = &c
	if att != nil {
		m.attempts[d.ID] = append(m.attempts[d.ID], att)
	}
	return nil
}

func (m *MemStore) Snapshot(id uuid.UUID) *domain.Delivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.deliveries[id]
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

func (m *MemStore) Get(_ context.Context, merchantID, id uuid.UUID) (*domain.Delivery, error) {
	d := m.Snapshot(id)
	if d == nil || d.MerchantID != merchantID {
		return nil, domain.ErrDeliveryNotFound
	}
	m.mu.Lock()
	if ev, ok := m.events[d.EventID]; ok {
		d.EventType, d.EventPayload, d.Livemode = ev.Type, ev.Payload, ev.Livemode
	}
	m.mu.Unlock()
	return d, nil
}

func (m *MemStore) ListAttempts(_ context.Context, id uuid.UUID) ([]*domain.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*domain.Attempt(nil), m.attempts[id]...), nil
}

func (m *MemStore) List(_ context.Context, f app.DeliveryFilter) (*app.DeliveryPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*domain.Delivery
	for _, d := range m.deliveries {
		if d.MerchantID != f.MerchantID {
			continue
		}
		if len(f.Statuses) > 0 {
			ok := false
			for _, s := range f.Statuses {
				ok = ok || s == d.Status
			}
			if !ok {
				continue
			}
		}
		c := *d
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return &app.DeliveryPage{Deliveries: out}, nil
}

func (m *MemStore) ReapStuck(_ context.Context, before, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, d := range m.deliveries {
		if d.Status == domain.StatusInFlight && d.UpdatedAt.Before(before) {
			_ = d.Reap(now)
			n++
		}
	}
	return n, nil
}

func (m *MemStore) CancelForEndpoint(_ context.Context, endpointID uuid.UUID, now time.Time, reason string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, d := range m.deliveries {
		if d.EndpointID == endpointID && d.Cancel(now, reason) {
			n++
		}
	}
	return n, nil
}

func (m *MemStore) CountByStatus(s domain.DeliveryStatus) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, d := range m.deliveries {
		if d.Status == s {
			n++
		}
	}
	return n
}

// MemEndpoints 為記憶體版 EndpointSource + EndpointDisabler。
type MemEndpoints struct {
	mu          sync.Mutex
	ByMerch     map[uuid.UUID][]*domain.Endpoint
	Err         error
	Calls       int
	Disabled    []uuid.UUID
	Invalidated int
}

func NewMemEndpoints() *MemEndpoints {
	return &MemEndpoints{ByMerch: map[uuid.UUID][]*domain.Endpoint{}}
}

func (m *MemEndpoints) Add(ep *domain.Endpoint) {
	m.ByMerch[ep.MerchantID] = append(m.ByMerch[ep.MerchantID], ep)
}

func (m *MemEndpoints) ListEndpoints(_ context.Context, merchantID uuid.UUID) ([]*domain.Endpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls++
	if m.Err != nil {
		return nil, m.Err
	}
	return m.ByMerch[merchantID], nil
}

func (m *MemEndpoints) GetEndpoint(ctx context.Context, merchantID, endpointID uuid.UUID) (*domain.Endpoint, error) {
	eps, err := m.ListEndpoints(ctx, merchantID)
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

func (m *MemEndpoints) DisableEndpoint(_ context.Context, _ uuid.UUID, endpointID uuid.UUID, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Disabled = append(m.Disabled, endpointID)
	for _, eps := range m.ByMerch {
		for _, ep := range eps {
			if ep.ID == endpointID {
				ep.Status = domain.EndpointDisabled
			}
		}
	}
	return nil
}

func (m *MemEndpoints) Invalidate(uuid.UUID) {
	m.mu.Lock()
	m.Invalidated++
	m.mu.Unlock()
}

// ScriptedSender 依序回傳預設結果，並記錄收到的請求。
type ScriptedSender struct {
	mu       sync.Mutex
	Outcomes []domain.Outcome
	Requests []app.SendRequest
}

func (s *ScriptedSender) Send(_ context.Context, req app.SendRequest) domain.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, req)
	if len(s.Outcomes) == 0 {
		return domain.Outcome{StatusCode: 200}
	}
	o := s.Outcomes[0]
	if len(s.Outcomes) > 1 {
		s.Outcomes = s.Outcomes[1:]
	}
	return o
}

// FakeClock 可手動撥動。
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// All 回傳所有 delivery 的複本。
func (m *MemStore) All() []*domain.Delivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*domain.Delivery, 0, len(m.deliveries))
	for _, d := range m.deliveries {
		c := *d
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// First 回傳最早建立的 delivery（測試常只有一筆）。
func (m *MemStore) First() *domain.Delivery {
	all := m.All()
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

// EventCount 回傳已記錄的事件數。
func (m *MemStore) EventCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

// NewFakeClock 建立固定時間的時鐘。
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{t: t} }

var (
	_ app.Transactor       = (*MemStore)(nil)
	_ app.Inbox            = (*MemStore)(nil)
	_ app.DeliveryRepo     = (*MemStore)(nil)
	_ app.EventRepo        = (*MemEventRepo)(nil)
	_ app.EndpointSource   = (*MemEndpoints)(nil)
	_ app.EndpointDisabler = (*MemEndpoints)(nil)
	_ app.HTTPSender       = (*ScriptedSender)(nil)
	_ app.Clock            = (*FakeClock)(nil)
)

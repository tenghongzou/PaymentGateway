// Package porttest 提供 app ports 的 in-memory fake（docs/09 §2.2），供 use case 單元測試與 gRPC adapter 測試使用。
package porttest

import (
	"context"
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
)

// Store 把所有 fake 綁在同一組記憶體資料上（ListUnsettled 需要同時看 lines 與 records）。
type Store struct {
	mu            sync.Mutex
	Files         map[uuid.UUID]*domain.SettlementFile
	Lines         map[uuid.UUID][]domain.SettlementLine // by file id
	Records       map[uuid.UUID]*domain.PaymentRecord
	Runs          map[uuid.UUID]*domain.Run
	Discrepancies map[uuid.UUID]*domain.Discrepancy
	Processed     map[string]bool // event_id|consumer
	Outbox        []app.OutboxMessage

	// TxCount 統計 WithinTx 呼叫次數（含巢狀重用）。
	TxCount atomic.Int64
	// FailNextUpdate 讓下一次 Update（任一 repo）回傳樂觀鎖衝突。
	FailNextUpdate atomic.Bool
	// Errs 可為特定操作注入錯誤（key 例："files.create"、"records.upsert"）。
	Errs map[string]error
}

// NewStore 建立空的 Store。
func NewStore() *Store {
	return &Store{
		Files:         map[uuid.UUID]*domain.SettlementFile{},
		Lines:         map[uuid.UUID][]domain.SettlementLine{},
		Records:       map[uuid.UUID]*domain.PaymentRecord{},
		Runs:          map[uuid.UUID]*domain.Run{},
		Discrepancies: map[uuid.UUID]*domain.Discrepancy{},
		Processed:     map[string]bool{},
		Errs:          map[string]error{},
	}
}

// Deps 回傳以此 Store 為後端的整組 app.Deps（Clock 預設固定時間，可覆寫）。
func (s *Store) Deps(clock app.Clock) app.Deps {
	return app.Deps{
		Tx: &Tx{s: s}, Files: &Files{s: s}, Lines: &Lines{s: s}, Records: &Records{s: s},
		Runs: &Runs{s: s}, Discrepancies: &Discrepancies{s: s}, Inbox: &Inbox{s: s}, Outbox: &Outbox{s: s},
		Clock: clock,
	}
}

func (s *Store) err(key string) error {
	if e, ok := s.Errs[key]; ok {
		delete(s.Errs, key)
		return e
	}
	return nil
}

func (s *Store) failUpdate() bool { return s.FailNextUpdate.CompareAndSwap(true, false) }

// OutboxByType 回傳特定 event_type 的 outbox 訊息。
func (s *Store) OutboxByType(eventType string) []app.OutboxMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []app.OutboxMessage
	for _, m := range s.Outbox {
		if m.EventType == eventType {
			out = append(out, m)
		}
	}
	return out
}

// AddRecord 直接放入一筆讀模型紀錄（測試前置）。
func (s *Store) AddRecord(r domain.PaymentRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := r
	s.Records[r.ID] = &rc
}

// AddDiscrepancy 直接放入一筆差異（測試前置）。
func (s *Store) AddDiscrepancy(d domain.Discrepancy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dc := d
	s.Discrepancies[d.ID] = &dc
}

// AddRun 直接放入一筆 run。
func (s *Store) AddRun(r domain.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := r
	s.Runs[r.ID] = &rc
}

// ---- Tx ----

type txKey struct{}

// Tx 實作 app.TxManager（無真正交易；僅標記 ctx 並計數）。
type Tx struct{ s *Store }

// WithinTx 實作 app.TxManager。
func (t *Tx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	t.s.TxCount.Add(1)
	if ctx.Value(txKey{}) != nil {
		return fn(ctx)
	}
	return fn(context.WithValue(ctx, txKey{}, true))
}

// InTx 回傳 ctx 是否在 WithinTx 內。
func InTx(ctx context.Context) bool { return ctx.Value(txKey{}) != nil }

// ---- Files ----

// Files 實作 app.FileRepo。
type Files struct{ s *Store }

// Create 實作 app.FileRepo。
func (r *Files) Create(_ context.Context, f *domain.SettlementFile) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.err("files.create"); err != nil {
		return err
	}
	for _, e := range r.s.Files {
		if e.FileHash == f.FileHash {
			return domain.ErrDuplicateFile
		}
	}
	c := *f
	r.s.Files[f.ID] = &c
	return nil
}

// GetByHash 實作 app.FileRepo。
func (r *Files) GetByHash(_ context.Context, hash string) (*domain.SettlementFile, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, e := range r.s.Files {
		if e.FileHash == hash {
			c := *e
			return &c, nil
		}
	}
	return nil, domain.ErrFileNotFound
}

// GetByID 實作 app.FileRepo。
func (r *Files) GetByID(_ context.Context, id uuid.UUID) (*domain.SettlementFile, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if e, ok := r.s.Files[id]; ok {
		c := *e
		return &c, nil
	}
	return nil, domain.ErrFileNotFound
}

// Update 實作 app.FileRepo。
func (r *Files) Update(_ context.Context, f *domain.SettlementFile) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.failUpdate() {
		return domain.ErrConcurrentModification
	}
	e, ok := r.s.Files[f.ID]
	if !ok {
		return domain.ErrFileNotFound
	}
	if e.Version != f.Version {
		return domain.ErrConcurrentModification
	}
	f.Version++
	c := *f
	r.s.Files[f.ID] = &c
	return nil
}

// ---- Lines ----

// Lines 實作 app.LineRepo。
type Lines struct{ s *Store }

// InsertBatch 實作 app.LineRepo。
func (r *Lines) InsertBatch(_ context.Context, lines []domain.SettlementLine) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.err("lines.insert"); err != nil {
		return err
	}
	for _, l := range lines {
		dup := false
		for _, e := range r.s.Lines[l.FileID] {
			if e.LineNo == l.LineNo {
				dup = true
				break
			}
		}
		if !dup {
			r.s.Lines[l.FileID] = append(r.s.Lines[l.FileID], l)
		}
	}
	return nil
}

// ListByFile 實作 app.LineRepo。
func (r *Lines) ListByFile(_ context.Context, fileID uuid.UUID) ([]domain.SettlementLine, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := append([]domain.SettlementLine(nil), r.s.Lines[fileID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].LineNo < out[j].LineNo })
	return out, nil
}

// ---- Records ----

// Records 實作 app.PaymentRecordRepo。
type Records struct{ s *Store }

// Upsert 實作 app.PaymentRecordRepo。
func (r *Records) Upsert(_ context.Context, rec *domain.PaymentRecord) (bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.err("records.upsert"); err != nil {
		return false, err
	}
	if e, ok := r.s.Records[rec.ID]; ok && e.SourceSeq > rec.SourceSeq {
		return false, nil
	}
	c := *rec
	r.s.Records[rec.ID] = &c
	return true, nil
}

// Get 實作 app.PaymentRecordRepo。
func (r *Records) Get(_ context.Context, id uuid.UUID) (*domain.PaymentRecord, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if e, ok := r.s.Records[id]; ok {
		c := *e
		return &c, nil
	}
	return nil, nil
}

// FindByProviderRefs 實作 app.PaymentRecordRepo。
func (r *Records) FindByProviderRefs(_ context.Context, provider string, refs []string) ([]domain.PaymentRecord, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	want := map[string]bool{}
	for _, ref := range refs {
		want[ref] = true
	}
	var out []domain.PaymentRecord
	for _, e := range r.s.Records {
		if e.Provider == provider && want[e.ProviderReference] {
			out = append(out, *e)
		}
	}
	return out, nil
}

// ListUnsettled 實作 app.PaymentRecordRepo：應結算、occurred_at < before、沒有任何 line 對上的紀錄。
func (r *Records) ListUnsettled(_ context.Context, provider string, before time.Time, limit int) ([]domain.PaymentRecord, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	settled := map[domain.MatchKey]bool{}
	for _, ls := range r.s.Lines {
		for _, l := range ls {
			if l.Provider == provider {
				settled[l.Key()] = true
			}
		}
	}
	var out []domain.PaymentRecord
	for _, e := range r.s.Records {
		if e.Provider != provider || !e.IsSettleable() || !e.OccurredAt.Before(before) || settled[e.Key()] {
			continue
		}
		out = append(out, *e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ---- Runs ----

// Runs 實作 app.RunRepo。
type Runs struct{ s *Store }

// Create 實作 app.RunRepo。
func (r *Runs) Create(_ context.Context, run *domain.Run) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.err("runs.create"); err != nil {
		return err
	}
	c := *run
	r.s.Runs[run.ID] = &c
	return nil
}

// Update 實作 app.RunRepo。
func (r *Runs) Update(_ context.Context, run *domain.Run) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.failUpdate() {
		return domain.ErrConcurrentModification
	}
	e, ok := r.s.Runs[run.ID]
	if !ok {
		return domain.ErrRunNotFound
	}
	// domain 方法已自行 Version++，fake 只檢查不倒退。
	if run.Version < e.Version {
		return domain.ErrConcurrentModification
	}
	c := *run
	r.s.Runs[run.ID] = &c
	return nil
}

// GetByID 實作 app.RunRepo。
func (r *Runs) GetByID(_ context.Context, id uuid.UUID) (*domain.Run, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if e, ok := r.s.Runs[id]; ok {
		c := *e
		return &c, nil
	}
	return nil, domain.ErrRunNotFound
}

// FindByFileID 實作 app.RunRepo。
func (r *Runs) FindByFileID(_ context.Context, fileID uuid.UUID) (*domain.Run, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var best *domain.Run
	for _, e := range r.s.Runs {
		if e.Summary.FileID == fileID.String() && (best == nil || e.CreatedAt.After(best.CreatedAt)) {
			best = e
		}
	}
	if best == nil {
		return nil, nil
	}
	c := *best
	return &c, nil
}

// List 實作 app.RunRepo（offset 游標）。
func (r *Runs) List(_ context.Context, f app.RunFilter) ([]domain.Run, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var all []domain.Run
	for _, e := range r.s.Runs {
		if f.Provider != "" && e.Provider != f.Provider {
			continue
		}
		if len(f.Statuses) > 0 && !containsRunStatus(f.Statuses, e.Status) {
			continue
		}
		if !f.DateFrom.IsZero() && e.PeriodStart.Before(f.DateFrom) {
			continue
		}
		if !f.DateTo.IsZero() && e.PeriodStart.After(f.DateTo) {
			continue
		}
		all = append(all, *e)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	return paginate(all, f.PageSize, f.PageToken)
}

// ---- Discrepancies ----

// Discrepancies 實作 app.DiscrepancyRepo。
type Discrepancies struct{ s *Store }

// InsertBatch 實作 app.DiscrepancyRepo。
func (r *Discrepancies) InsertBatch(_ context.Context, ds []domain.Discrepancy) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.err("discrepancies.insert"); err != nil {
		return err
	}
	for i := range ds {
		c := ds[i]
		r.s.Discrepancies[c.ID] = &c
	}
	return nil
}

// GetByID 實作 app.DiscrepancyRepo。
func (r *Discrepancies) GetByID(_ context.Context, id uuid.UUID) (*domain.Discrepancy, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if e, ok := r.s.Discrepancies[id]; ok {
		c := *e
		return &c, nil
	}
	return nil, domain.ErrDiscrepancyNotFound
}

// Update 實作 app.DiscrepancyRepo。
func (r *Discrepancies) Update(_ context.Context, d *domain.Discrepancy) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.failUpdate() {
		return domain.ErrConcurrentModification
	}
	e, ok := r.s.Discrepancies[d.ID]
	if !ok {
		return domain.ErrDiscrepancyNotFound
	}
	if d.Version < e.Version {
		return domain.ErrConcurrentModification
	}
	c := *d
	r.s.Discrepancies[d.ID] = &c
	return nil
}

// List 實作 app.DiscrepancyRepo（offset 游標）。
func (r *Discrepancies) List(_ context.Context, f app.DiscrepancyFilter) ([]domain.Discrepancy, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var all []domain.Discrepancy
	for _, e := range r.s.Discrepancies {
		if f.RunID != nil && e.RunID != *f.RunID {
			continue
		}
		if f.MerchantID != nil && (e.MerchantID == nil || *e.MerchantID != *f.MerchantID) {
			continue
		}
		if f.Provider != "" && e.Provider != f.Provider {
			continue
		}
		if len(f.Kinds) > 0 && !containsKind(f.Kinds, e.Kind) {
			continue
		}
		if len(f.Statuses) > 0 && !containsStatus(f.Statuses, e.Status) {
			continue
		}
		if f.PaymentID != "" && e.InternalReference != f.PaymentID {
			continue
		}
		if !f.CreatedAfter.IsZero() && !e.CreatedAt.After(f.CreatedAfter) {
			continue
		}
		if !f.CreatedBefore.IsZero() && !e.CreatedAt.Before(f.CreatedBefore) {
			continue
		}
		all = append(all, *e)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID.String() > all[j].ID.String()
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	return paginate(all, f.PageSize, f.PageToken)
}

// ExistsOpen 實作 app.DiscrepancyRepo。
func (r *Discrepancies) ExistsOpen(_ context.Context, provider string, kind domain.DiscrepancyKind, providerRef, internalRef string) (bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, e := range r.s.Discrepancies {
		if e.Provider != provider || e.Kind != kind || e.Status != domain.DiscrepancyOpen {
			continue
		}
		if (providerRef != "" && e.ProviderReference == providerRef) || (internalRef != "" && e.InternalReference == internalRef) {
			return true, nil
		}
	}
	return false, nil
}

// ---- Inbox / Outbox ----

// Inbox 實作 app.Inbox。
type Inbox struct{ s *Store }

// MarkProcessed 實作 app.Inbox。
func (i *Inbox) MarkProcessed(_ context.Context, eventID, consumer string) (bool, error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	k := eventID + "|" + consumer
	if i.s.Processed[k] {
		return true, nil
	}
	i.s.Processed[k] = true
	return false, nil
}

// Outbox 實作 app.OutboxStore。
type Outbox struct{ s *Store }

// Insert 實作 app.OutboxStore。
func (o *Outbox) Insert(ctx context.Context, msg app.OutboxMessage) (string, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if err := o.s.err("outbox.insert"); err != nil {
		return "", err
	}
	if !InTx(ctx) {
		panic("porttest: outbox.Insert called outside WithinTx")
	}
	o.s.Outbox = append(o.s.Outbox, msg)
	return uuid.NewString(), nil
}

// ---- Clock ----

// Clock 為可撥動的時鐘。
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock 建立固定在 t 的時鐘。
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now 實作 app.Clock。
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance 撥快時間。
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Set 設定時間。
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// ---- helpers ----

func paginate[T any](all []T, size int, token string) ([]T, string, error) {
	offset := 0
	if token != "" {
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return nil, "", domain.ErrInvalidTransition.WithMessage("invalid page_token")
		}
		offset, err = strconv.Atoi(strings.TrimPrefix(string(raw), "o:"))
		if err != nil || offset < 0 {
			return nil, "", domain.ErrInvalidTransition.WithMessage("invalid page_token")
		}
	}
	if size <= 0 {
		size = app.DefaultPageSize
	}
	if offset >= len(all) {
		return nil, "", nil
	}
	end := offset + size
	next := ""
	if end < len(all) {
		next = base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(end)))
	} else {
		end = len(all)
	}
	return all[offset:end], next, nil
}

func containsRunStatus(xs []domain.RunStatus, x domain.RunStatus) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func containsKind(xs []domain.DiscrepancyKind, x domain.DiscrepancyKind) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func containsStatus(xs []domain.DiscrepancyStatus, x domain.DiscrepancyStatus) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// 確保介面實作。
var (
	_ app.TxManager         = (*Tx)(nil)
	_ app.FileRepo          = (*Files)(nil)
	_ app.LineRepo          = (*Lines)(nil)
	_ app.PaymentRecordRepo = (*Records)(nil)
	_ app.RunRepo           = (*Runs)(nil)
	_ app.DiscrepancyRepo   = (*Discrepancies)(nil)
	_ app.Inbox             = (*Inbox)(nil)
	_ app.OutboxStore       = (*Outbox)(nil)
	_ app.Clock             = (*Clock)(nil)
)

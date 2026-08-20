package app

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// fakeStore 為記憶體版的全部 ports：交易以「快照 + 失敗時還原」模擬 rollback，
// 讓 use case 測試能驗證原子性（processed_events、journal、outbox 同進同退）。
type fakeStore struct {
	mu        sync.Mutex
	accounts  map[domain.AccountKey]*domain.Account
	journals  []*domain.Journal
	processed map[string]bool
	outbox    []OutboxMessage
	balances  domain.Balances
	now       time.Time
	// failInsert 讓 Insert 回傳指定錯誤（模擬 DB 拒絕）。
	failInsert error
	// failOutbox 讓 outbox Insert 失敗。
	failOutbox error
	// inTx 表示目前在交易內（repo 方法會檢查，確保 use case 走 RunInTx）。
	inTx bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts:  map[domain.AccountKey]*domain.Account{},
		processed: map[string]bool{},
		balances:  domain.Balances{},
		now:       time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fakeStore) Now() time.Time { return f.now }

type snapshot struct {
	accounts  map[domain.AccountKey]*domain.Account
	journals  []*domain.Journal
	processed map[string]bool
	outbox    []OutboxMessage
	balances  domain.Balances
}

func (f *fakeStore) snap() snapshot {
	s := snapshot{
		accounts:  make(map[domain.AccountKey]*domain.Account, len(f.accounts)),
		journals:  append([]*domain.Journal(nil), f.journals...),
		processed: make(map[string]bool, len(f.processed)),
		outbox:    append([]OutboxMessage(nil), f.outbox...),
		balances:  f.balances.Clone(),
	}
	for k, v := range f.accounts {
		c := *v
		s.accounts[k] = &c
	}
	for k, v := range f.processed {
		s.processed[k] = v
	}
	return s
}

func (f *fakeStore) restore(s snapshot) {
	f.accounts, f.journals, f.processed, f.outbox, f.balances = s.accounts, s.journals, s.processed, s.outbox, s.balances
}

// RunInTx 實作 TxRunner。
func (f *fakeStore) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.snap()
	f.inTx = true
	err := fn(ctx)
	f.inTx = false
	if err != nil {
		f.restore(s)
	}
	return err
}

// --- AccountRepo ---

func (f *fakeStore) EnsureAccount(_ context.Context, key domain.AccountKey) (*domain.Account, bool, error) {
	if a, ok := f.accounts[key]; ok {
		c := *a
		return &c, false, nil
	}
	a, err := domain.NewAccount(key)
	if err != nil {
		return nil, false, err
	}
	a.ID = ids.NewUUID()
	a.CreatedAt, a.UpdatedAt = f.now, f.now
	f.accounts[key] = a
	c := *a
	return &c, true, nil
}

func (f *fakeStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Account, error) {
	for _, a := range f.accounts {
		if a.ID == id {
			c := *a
			return &c, nil
		}
	}
	return nil, domain.ErrAccountNotFound
}

func (f *fakeStore) GetByKey(_ context.Context, key domain.AccountKey) (*domain.Account, error) {
	if a, ok := f.accounts[key]; ok {
		c := *a
		return &c, nil
	}
	return nil, domain.ErrAccountNotFound
}

func (f *fakeStore) List(_ context.Context, fl AccountFilter, page Page) ([]*domain.Account, *Cursor, error) {
	var all []*domain.Account
	for _, a := range f.accounts {
		if fl.MerchantID != nil && a.Key.MerchantID != *fl.MerchantID {
			continue
		}
		if fl.SystemOnly && !a.Key.IsSystem() {
			continue
		}
		if fl.Kind != "" && a.Kind() != fl.Kind {
			continue
		}
		if fl.Qualifier != "" && a.Key.Qualifier() != fl.Qualifier {
			continue
		}
		if fl.Currency != "" && a.Key.Currency != fl.Currency {
			continue
		}
		if fl.Livemode != nil && a.Key.Livemode != *fl.Livemode {
			continue
		}
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID.String() < all[j].ID.String() })
	start := 0
	if page.After != nil {
		for i, a := range all {
			if a.ID == page.After.ID {
				start = i + 1
			}
		}
	}
	end := min(start+page.Limit, len(all))
	out := all[start:end]
	var next *Cursor
	if end < len(all) {
		next = &Cursor{At: out[len(out)-1].CreatedAt, ID: out[len(out)-1].ID}
	}
	return out, next, nil
}

// --- JournalRepo（frozen account simulation 透過 freeze()）---

func (f *fakeStore) freeze(key domain.AccountKey) {
	if a, ok := f.accounts[key]; ok {
		a.Status = domain.AccountFrozen
	}
}

func (f *fakeStore) Insert(_ context.Context, j *domain.Journal) error {
	if !f.inTx {
		return errors.New("fake: Insert outside transaction")
	}
	if f.failInsert != nil {
		return f.failInsert
	}
	for _, e := range f.journals {
		if e.EventID == j.EventID {
			return domain.ErrDuplicateEvent
		}
	}
	// 模擬 DB trigger：帳戶必須 active、幣別相符、借貸平衡。
	for _, e := range j.Entries {
		a, ok := f.accounts[e.Account]
		if !ok || a.ID != e.AccountID {
			return domain.ErrAccountNotFound
		}
		if !a.CanPost() {
			return domain.ErrAccountInactive
		}
	}
	if err := f.balances.Apply(j); err != nil {
		return err
	}
	c := *j
	c.Entries = append([]domain.Entry(nil), j.Entries...)
	f.journals = append(f.journals, &c)
	return nil
}

func (f *fakeStore) withReversedBy(j *domain.Journal) *domain.Journal {
	c := *j
	c.Entries = append([]domain.Entry(nil), j.Entries...)
	for _, o := range f.journals {
		if o.ReversalOf != nil && *o.ReversalOf == j.ID {
			id := o.ID
			c.ReversedBy = &id
		}
	}
	return &c
}

func (f *fakeStore) GetByEventID(_ context.Context, eventID uuid.UUID) (*domain.Journal, error) {
	for _, j := range f.journals {
		if j.EventID == eventID {
			return f.withReversedBy(j), nil
		}
	}
	return nil, domain.ErrJournalNotFound
}

func (f *fakeStore) GetJournalByID(_ context.Context, id uuid.UUID) (*domain.Journal, error) {
	for _, j := range f.journals {
		if j.ID == id {
			return f.withReversedBy(j), nil
		}
	}
	return nil, domain.ErrJournalNotFound
}

func (f *fakeStore) ListJournals(_ context.Context, fl JournalFilter, page Page) ([]*domain.Journal, *Cursor, error) {
	var all []*domain.Journal
	for _, j := range f.journals {
		if fl.MerchantID != nil && j.MerchantID != *fl.MerchantID {
			continue
		}
		if fl.ReferenceType != "" && j.ReferenceType != fl.ReferenceType {
			continue
		}
		if fl.ReferenceID != "" && j.ReferenceID != fl.ReferenceID {
			continue
		}
		if fl.Template != "" && j.Template != fl.Template {
			continue
		}
		if fl.SourceType != "" && j.SourceType != fl.SourceType {
			continue
		}
		if fl.Currency != "" && j.Currency() != fl.Currency {
			continue
		}
		if fl.Livemode != nil && j.Livemode != *fl.Livemode {
			continue
		}
		if fl.AccountID != nil {
			hit := false
			for _, e := range j.Entries {
				if e.AccountID == *fl.AccountID {
					hit = true
				}
			}
			if !hit {
				continue
			}
		}
		all = append(all, f.withReversedBy(j))
	}
	// posted_at DESC（插入順序反轉）
	for i, k := 0, len(all)-1; i < k; i, k = i+1, k-1 {
		all[i], all[k] = all[k], all[i]
	}
	start := 0
	if page.After != nil {
		for i, j := range all {
			if j.ID == page.After.ID {
				start = i + 1
			}
		}
	}
	end := min(start+page.Limit, len(all))
	out := all[start:end]
	var next *Cursor
	if end < len(all) {
		next = &Cursor{At: out[len(out)-1].PostedAt, ID: out[len(out)-1].ID}
	}
	return out, next, nil
}

// journalRepo 把 fakeStore 包成 JournalRepo（避免 GetByID / List 與 AccountRepo 撞名）。
type journalRepo struct{ f *fakeStore }

func (r journalRepo) Insert(ctx context.Context, j *domain.Journal) error { return r.f.Insert(ctx, j) }
func (r journalRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Journal, error) {
	return r.f.GetJournalByID(ctx, id)
}
func (r journalRepo) GetByEventID(ctx context.Context, id uuid.UUID) (*domain.Journal, error) {
	return r.f.GetByEventID(ctx, id)
}
func (r journalRepo) List(ctx context.Context, fl JournalFilter, p Page) ([]*domain.Journal, *Cursor, error) {
	return r.f.ListJournals(ctx, fl, p)
}

// --- BalanceRepo ---

type balanceRepo struct{ f *fakeStore }

func (r balanceRepo) GetByAccount(_ context.Context, accountID uuid.UUID) (*domain.Balance, error) {
	for key, a := range r.f.accounts {
		if a.ID != accountID {
			continue
		}
		b := &domain.Balance{AccountID: a.ID, Account: key, Type: a.Type, NormalBalance: a.NormalBalance, Balance: r.f.balances.Of(key), UpdatedAt: r.f.now}
		for _, j := range r.f.journals {
			for _, e := range j.Entries {
				if e.AccountID == accountID {
					b.EntryCount++
					if e.Direction == domain.Debit {
						b.TotalDebit += e.Amount.AmountMinor
					} else {
						b.TotalCredit += e.Amount.AmountMinor
					}
					id := j.ID
					b.AsOfJournalID = &id
				}
			}
		}
		return b, nil
	}
	return nil, domain.ErrAccountNotFound
}

func (r balanceRepo) ListByMerchant(ctx context.Context, merchantID uuid.UUID, currency string, livemode bool) ([]*domain.Balance, error) {
	var out []*domain.Balance
	for key, a := range r.f.accounts {
		if key.MerchantID != merchantID || key.Livemode != livemode || (currency != "" && key.Currency != currency) {
			continue
		}
		b, err := r.GetByAccount(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// --- Inbox / Outbox ---

type inbox struct{ f *fakeStore }

func (i inbox) MarkProcessed(_ context.Context, eventID uuid.UUID, consumer string) (bool, error) {
	if !i.f.inTx {
		return false, errors.New("fake: MarkProcessed outside transaction")
	}
	k := eventID.String() + "|" + consumer
	if i.f.processed[k] {
		return true, nil
	}
	i.f.processed[k] = true
	return false, nil
}

type outboxStore struct{ f *fakeStore }

func (o outboxStore) Insert(_ context.Context, msg OutboxMessage) (string, error) {
	if !o.f.inTx {
		return "", errors.New("fake: outbox Insert outside transaction")
	}
	if o.f.failOutbox != nil {
		return "", o.f.failOutbox
	}
	o.f.outbox = append(o.f.outbox, msg)
	return ids.NewUUID().String(), nil
}

// newTestService 組裝 Service 與 fake ports。
func newTestService(pol domain.Policy) (*Service, *fakeStore) {
	f := newFakeStore()
	svc := NewService(Deps{
		Tx: f, Accounts: f, Journals: journalRepo{f}, Balances: balanceRepo{f},
		Inbox: inbox{f}, Outbox: outboxStore{f}, Clock: f, Policy: pol,
		Logger: slog.New(slog.DiscardHandler),
	})
	return svc, f
}

package grpc

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	ledgerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/ledger/v1"
	"github.com/tenghongzou/paymentgateway/internal/ledger/app"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// ---- 最小 fake ports（單執行緒、無交易語意；只為驗證 proto ↔ app 映射與錯誤碼）----

type memStore struct {
	accounts map[domain.AccountKey]*domain.Account
	journals []*domain.Journal
	balances domain.Balances
	outbox   int
}

func newMem() *memStore {
	return &memStore{accounts: map[domain.AccountKey]*domain.Account{}, balances: domain.Balances{}}
}

func (m *memStore) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}
func (m *memStore) Now() time.Time { return time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC) }

func (m *memStore) EnsureAccount(_ context.Context, key domain.AccountKey) (*domain.Account, bool, error) {
	if a, ok := m.accounts[key]; ok {
		return a, false, nil
	}
	a, err := domain.NewAccount(key)
	if err != nil {
		return nil, false, err
	}
	a.ID = ids.NewUUID()
	a.CreatedAt = m.Now()
	m.accounts[key] = a
	return a, true, nil
}

func (m *memStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Account, error) {
	for _, a := range m.accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, domain.ErrAccountNotFound
}

func (m *memStore) GetByKey(_ context.Context, key domain.AccountKey) (*domain.Account, error) {
	if a, ok := m.accounts[key]; ok {
		return a, nil
	}
	return nil, domain.ErrAccountNotFound
}

func (m *memStore) List(_ context.Context, f app.AccountFilter, _ app.Page) ([]*domain.Account, *app.Cursor, error) {
	var out []*domain.Account
	for _, a := range m.accounts {
		if f.MerchantID != nil && a.Key.MerchantID != *f.MerchantID {
			continue
		}
		out = append(out, a)
	}
	return out, nil, nil
}

type memJournals struct{ m *memStore }

func (r memJournals) Insert(_ context.Context, j *domain.Journal) error {
	for _, e := range r.m.journals {
		if e.EventID == j.EventID {
			return domain.ErrDuplicateEvent
		}
	}
	if err := r.m.balances.Apply(j); err != nil {
		return err
	}
	r.m.journals = append(r.m.journals, j)
	return nil
}
func (r memJournals) GetByID(_ context.Context, id uuid.UUID) (*domain.Journal, error) {
	for _, j := range r.m.journals {
		if j.ID == id {
			return j, nil
		}
	}
	return nil, domain.ErrJournalNotFound
}
func (r memJournals) GetByEventID(_ context.Context, id uuid.UUID) (*domain.Journal, error) {
	for _, j := range r.m.journals {
		if j.EventID == id {
			return j, nil
		}
	}
	return nil, domain.ErrJournalNotFound
}
func (r memJournals) List(_ context.Context, f app.JournalFilter, _ app.Page) ([]*domain.Journal, *app.Cursor, error) {
	var out []*domain.Journal
	for _, j := range r.m.journals {
		if f.MerchantID != nil && j.MerchantID != *f.MerchantID {
			continue
		}
		out = append(out, j)
	}
	return out, nil, nil
}

type memBalances struct{ m *memStore }

func (r memBalances) GetByAccount(_ context.Context, id uuid.UUID) (*domain.Balance, error) {
	for key, a := range r.m.accounts {
		if a.ID == id {
			return &domain.Balance{AccountID: id, Account: key, Type: a.Type, NormalBalance: a.NormalBalance, Balance: r.m.balances.Of(key), UpdatedAt: r.m.Now()}, nil
		}
	}
	return nil, domain.ErrAccountNotFound
}
func (r memBalances) ListByMerchant(ctx context.Context, merchantID uuid.UUID, currency string, livemode bool) ([]*domain.Balance, error) {
	var out []*domain.Balance
	for key, a := range r.m.accounts {
		if key.MerchantID == merchantID && key.Livemode == livemode && (currency == "" || key.Currency == currency) {
			b, err := r.GetByAccount(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
	}
	return out, nil
}

type memInbox struct{}

func (memInbox) MarkProcessed(context.Context, uuid.UUID, string) (bool, error) { return false, nil }

type memOutbox struct{ m *memStore }

func (o memOutbox) Insert(context.Context, app.OutboxMessage) (string, error) {
	o.m.outbox++
	return uuid.NewString(), nil
}

// startServer 以 bufconn 起 gRPC server（含 grpcx interceptors）並回傳 client。
func startServer(t *testing.T) (ledgerv1.LedgerServiceClient, *memStore) {
	t.Helper()
	m := newMem()
	svc := app.NewService(app.Deps{
		Tx: m, Accounts: m, Journals: memJournals{m}, Balances: memBalances{m}, Inbox: memInbox{}, Outbox: memOutbox{m},
		Clock: m, Logger: slog.New(slog.DiscardHandler),
	})
	srv, _ := grpcx.NewServer(grpcx.ServerOptions{Logger: slog.New(slog.DiscardHandler)})
	NewServer(svc).Register(srv)
	lis := bufconn.Listen(1 << 20)
	go func() {
		// Stop() 後 Serve 回 nil；非 nil 代表 server 起不來，直接讓測試炸掉。
		if err := srv.Serve(lis); err != nil {
			panic("ledger grpc test server: " + err.Error())
		}
	}()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return ledgerv1.NewLedgerServiceClient(conn), m
}

func detailCode(t *testing.T, err error) (codes.Code, string) {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status, got %v", err)
	var code string
	if d := grpcx.ErrorDetailFromStatus(st); d != nil {
		code = d.GetCode()
	}
	return st.Code(), code
}

func TestServer_AllRPCs(t *testing.T) {
	client, m := startServer(t)
	ctx := context.Background()
	merchant := ids.New(ids.PrefixMerchant)

	// CreateAccount（冪等）
	created, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{
		Type: ledgerv1.AccountType_ACCOUNT_TYPE_MERCHANT_PAYABLE, MerchantId: merchant, Currency: "TWD", Livemode: true,
	})
	require.NoError(t, err)
	assert.False(t, created.GetAlreadyExisted())
	assert.True(t, ids.HasPrefix(created.GetAccount().GetId(), ids.PrefixAccount))
	assert.Equal(t, merchant, created.GetAccount().GetMerchantId())
	assert.Equal(t, ledgerv1.AccountStatus_ACCOUNT_STATUS_ACTIVE, created.GetAccount().GetStatus())
	again, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{
		Type: ledgerv1.AccountType_ACCOUNT_TYPE_MERCHANT_PAYABLE, MerchantId: merchant, Currency: "TWD", Livemode: true,
	})
	require.NoError(t, err)
	assert.True(t, again.GetAlreadyExisted())
	assert.Equal(t, created.GetAccount().GetId(), again.GetAccount().GetId())

	psp, err := client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{
		Type: ledgerv1.AccountType_ACCOUNT_TYPE_PSP_RECEIVABLE, Provider: "stripe", Currency: "TWD", Livemode: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "stripe", psp.GetAccount().GetProvider())
	assert.Empty(t, psp.GetAccount().GetMerchantId())

	// 驗證錯誤：psp_receivable 缺 provider → InvalidArgument；系統帳戶帶 merchant → InvalidArgument
	_, err = client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Type: ledgerv1.AccountType_ACCOUNT_TYPE_PSP_RECEIVABLE, Currency: "TWD"})
	code, _ := detailCode(t, err)
	assert.Equal(t, codes.InvalidArgument, code)
	_, err = client.CreateAccount(ctx, &ledgerv1.CreateAccountRequest{Type: ledgerv1.AccountType_ACCOUNT_TYPE_FEE_REVENUE, MerchantId: merchant, Currency: "TWD"})
	code, _ = detailCode(t, err)
	assert.Equal(t, codes.InvalidArgument, code)

	// GetAccount
	got, err := client.GetAccount(ctx, &ledgerv1.GetAccountRequest{AccountId: created.GetAccount().GetId()})
	require.NoError(t, err)
	assert.Equal(t, ledgerv1.AccountType_ACCOUNT_TYPE_MERCHANT_PAYABLE, got.GetAccount().GetType())
	_, err = client.GetAccount(ctx, &ledgerv1.GetAccountRequest{AccountId: ids.New(ids.PrefixAccount)})
	code, ecode := detailCode(t, err)
	assert.Equal(t, codes.NotFound, code)
	assert.Equal(t, "resource_missing", ecode)
	_, err = client.GetAccount(ctx, &ledgerv1.GetAccountRequest{AccountId: "nope"})
	code, _ = detailCode(t, err)
	assert.Equal(t, codes.InvalidArgument, code)

	// ListAccounts
	list, err := client.ListAccounts(ctx, &ledgerv1.ListAccountsRequest{MerchantId: merchant})
	require.NoError(t, err)
	assert.Len(t, list.GetAccounts(), 1)
	assert.False(t, list.GetPage().GetHasMore())
	_, err = client.ListAccounts(ctx, &ledgerv1.ListAccountsRequest{Page: &commonv1.PageRequest{PageToken: "bad"}})
	code, _ = detailCode(t, err)
	assert.Equal(t, codes.InvalidArgument, code)

	// PostJournal：不平衡 → InvalidArgument / journal_unbalanced
	mpID, pspID := created.GetAccount().GetId(), psp.GetAccount().GetId()
	_, err = client.PostJournal(ctx, &ledgerv1.PostJournalRequest{
		IdempotencyKey: "adj-1", MerchantId: merchant, SourceType: ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_MANUAL_ADJUSTMENT,
		Description: "oops", Livemode: true,
		Entries: []*ledgerv1.EntryInput{
			{AccountId: pspID, Direction: ledgerv1.EntryDirection_ENTRY_DIRECTION_DEBIT, Amount: &commonv1.Money{AmountMinor: 100, Currency: "TWD"}},
			{AccountId: mpID, Direction: ledgerv1.EntryDirection_ENTRY_DIRECTION_CREDIT, Amount: &commonv1.Money{AmountMinor: 90, Currency: "TWD"}},
		},
	})
	code, ecode = detailCode(t, err)
	assert.Equal(t, codes.InvalidArgument, code)
	assert.Equal(t, "journal_unbalanced", ecode)

	// PAYMENT_EVENT 不可由 gRPC 寫入
	_, err = client.PostJournal(ctx, &ledgerv1.PostJournalRequest{
		IdempotencyKey: "adj-x", SourceType: ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_PAYMENT_EVENT, Description: "x",
		Entries: []*ledgerv1.EntryInput{{AccountId: pspID}, {AccountId: mpID}},
	})
	code, _ = detailCode(t, err)
	assert.Equal(t, codes.InvalidArgument, code)

	// 正常過帳 + 冪等重放
	req := &ledgerv1.PostJournalRequest{
		IdempotencyKey: "adj-2", MerchantId: merchant, SourceType: ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_MANUAL_ADJUSTMENT,
		SourceId: "ticket-9", Description: "manual credit", Livemode: true, Metadata: map[string]string{"ticket": "9"},
		Entries: []*ledgerv1.EntryInput{
			{AccountId: pspID, Direction: ledgerv1.EntryDirection_ENTRY_DIRECTION_DEBIT, Amount: &commonv1.Money{AmountMinor: 100, Currency: "TWD"}},
			{AccountId: mpID, Direction: ledgerv1.EntryDirection_ENTRY_DIRECTION_CREDIT, Amount: &commonv1.Money{AmountMinor: 100, Currency: "TWD"}},
		},
	}
	posted, err := client.PostJournal(ctx, req)
	require.NoError(t, err)
	assert.False(t, posted.GetIdempotentReplayed())
	j := posted.GetJournal()
	assert.True(t, ids.HasPrefix(j.GetId(), ids.PrefixJournal))
	assert.Equal(t, merchant, j.GetMerchantId())
	assert.Equal(t, "adjustment", j.GetReferenceType())
	assert.Equal(t, "ticket-9", j.GetReferenceId())
	assert.Equal(t, "TWD", j.GetCurrency())
	assert.Equal(t, "9", j.GetMetadata()["ticket"])
	require.Len(t, j.GetEntries(), 2)
	assert.Equal(t, ledgerv1.AccountType_ACCOUNT_TYPE_PSP_RECEIVABLE, j.GetEntries()[0].GetAccountType())
	assert.Equal(t, 1, m.outbox)
	replay, err := client.PostJournal(ctx, req)
	require.NoError(t, err)
	assert.True(t, replay.GetIdempotentReplayed())
	assert.Equal(t, j.GetId(), replay.GetJournal().GetId())

	// GetJournal（商戶隔離）
	gj, err := client.GetJournal(ctx, &ledgerv1.GetJournalRequest{JournalId: j.GetId(), MerchantId: merchant})
	require.NoError(t, err)
	assert.Equal(t, j.GetId(), gj.GetJournal().GetId())
	_, err = client.GetJournal(ctx, &ledgerv1.GetJournalRequest{JournalId: j.GetId(), MerchantId: ids.New(ids.PrefixMerchant)})
	code, _ = detailCode(t, err)
	assert.Equal(t, codes.NotFound, code)

	// ListJournals
	lj, err := client.ListJournals(ctx, &ledgerv1.ListJournalsRequest{MerchantId: merchant, Page: &commonv1.PageRequest{PageSize: 10}})
	require.NoError(t, err)
	assert.Len(t, lj.GetJournals(), 1)

	// 沖銷（reverses_journal_id）
	rev, err := client.PostJournal(ctx, &ledgerv1.PostJournalRequest{
		IdempotencyKey: "rev-1", MerchantId: merchant, SourceType: ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_REVERSAL,
		Description: "undo", Livemode: true, ReversesJournalId: j.GetId(),
		Entries: []*ledgerv1.EntryInput{
			{AccountId: pspID, Direction: ledgerv1.EntryDirection_ENTRY_DIRECTION_CREDIT, Amount: &commonv1.Money{AmountMinor: 100, Currency: "TWD"}},
			{AccountId: mpID, Direction: ledgerv1.EntryDirection_ENTRY_DIRECTION_DEBIT, Amount: &commonv1.Money{AmountMinor: 100, Currency: "TWD"}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_REVERSAL, rev.GetJournal().GetSourceType())
	assert.Equal(t, j.GetId(), rev.GetJournal().GetMetadata()["reversal_of_journal_id"])

	// GetBalance：帳戶 / 商戶
	bal, err := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{Target: &ledgerv1.GetBalanceRequest_AccountId{AccountId: mpID}})
	require.NoError(t, err)
	assert.Equal(t, int64(0), bal.GetBalance().GetBalanceMinor())
	assert.Equal(t, ledgerv1.AccountType_ACCOUNT_TYPE_MERCHANT_PAYABLE, bal.GetBalance().GetAccountType())
	mb, err := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{Target: &ledgerv1.GetBalanceRequest_Merchant{
		Merchant: &ledgerv1.MerchantCurrency{MerchantId: merchant, Currency: "TWD", Livemode: true}}})
	require.NoError(t, err)
	require.Len(t, mb.GetMerchantBalances(), 1)
	assert.Equal(t, merchant, mb.GetMerchantBalances()[0].GetMerchantId())
	_, err = client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{})
	code, _ = detailCode(t, err)
	assert.Equal(t, codes.InvalidArgument, code)
}

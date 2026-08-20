// Package app 為 ledger-service 的應用層：use cases（過帳、查詢、消費 payment.events）與 ports（repo / inbox / outbox / clock 介面）。
//
// 規則：app 不 import adapter；所有 DB 存取經由 ports，交易邊界由 TxRunner 控制、交易以 context 傳遞給 repo。
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
)

// 消費者 / 事件名稱常數。
const (
	// ConsumerPaymentEvents 為 processed_events.consumer 的值（docs/05 §8、migrations/ledger/0003）。
	ConsumerPaymentEvents = "ledger.payment-consumer"
	// EventJournalPosted 為寫入 outbox 的事件類型（topic ledger.events）。
	EventJournalPosted = "ledger.journal.posted"
	// AggregateJournal 為 outbox.aggregate_type。
	AggregateJournal = "journal"
)

// TxRunner 提供交易邊界：fn 內的 repo 呼叫透過 ctx 取得同一個交易；fn 回錯誤則 rollback。
type TxRunner interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Cursor 為 keyset 分頁游標（排序鍵 + tie-breaker id）。
type Cursor struct {
	At time.Time
	ID uuid.UUID
}

// Page 為分頁參數（Limit 已正規化：1..100）。
type Page struct {
	Limit int
	After *Cursor
}

// AccountFilter 為 ListAccounts 的篩選條件。
type AccountFilter struct {
	// MerchantID 非 nil 時只列該商戶；SystemOnly 為 true 時只列系統帳戶。
	MerchantID *uuid.UUID
	SystemOnly bool
	Kind       domain.Kind
	// Qualifier 為 code 後綴（provider / 銀行帳戶）。
	Qualifier string
	Currency  string
	Livemode  *bool
}

// JournalFilter 為 ListJournals 的篩選條件。
type JournalFilter struct {
	MerchantID    *uuid.UUID
	AccountID     *uuid.UUID
	ReferenceType domain.ReferenceType
	ReferenceID   string
	SourceType    domain.SourceType
	Template      string
	Currency      string
	PostedAfter   *time.Time // 含
	PostedBefore  *time.Time // 不含
	Livemode      *bool
}

// AccountRepo 為 accounts 表的存取介面。
type AccountRepo interface {
	// EnsureAccount 以 lazy create 取得帳戶（INSERT ... ON CONFLICT DO NOTHING 後 SELECT）；created 表示本次新建。
	EnsureAccount(ctx context.Context, key domain.AccountKey) (acct *domain.Account, created bool, err error)
	// GetByID 依 uuid 取得帳戶；不存在回 domain.ErrAccountNotFound。
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Account, error)
	// GetByKey 依自然鍵取得帳戶；不存在回 domain.ErrAccountNotFound。
	GetByKey(ctx context.Context, key domain.AccountKey) (*domain.Account, error)
	// List 依條件列出帳戶（created_at, id 升冪），回傳下一頁游標（無則 nil）。
	List(ctx context.Context, f AccountFilter, page Page) ([]*domain.Account, *Cursor, error)
}

// JournalRepo 為 journals + entries 的存取介面（append-only：沒有 Update / Delete）。
type JournalRepo interface {
	// Insert 在同一交易內寫入 journal 與全部 entries（借貸平衡由 DB deferred trigger 於 commit 時再驗一次）。
	// event_id 重複回 domain.ErrDuplicateEvent（此時交易已失效，呼叫端需 rollback 後另行讀取）。
	Insert(ctx context.Context, j *domain.Journal) error
	// GetByID 取得 journal（含 entries、ReversedBy）；不存在回 domain.ErrJournalNotFound。
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Journal, error)
	// GetByEventID 以冪等鍵取得 journal；不存在回 domain.ErrJournalNotFound。
	GetByEventID(ctx context.Context, eventID uuid.UUID) (*domain.Journal, error)
	// List 依條件列出 journal（posted_at DESC, id DESC），回傳下一頁游標。
	List(ctx context.Context, f JournalFilter, page Page) ([]*domain.Journal, *Cursor, error)
}

// BalanceRepo 為 balances 讀模型的存取介面。
type BalanceRepo interface {
	// GetByAccount 取得單一帳戶餘額（含借貸合計）；帳戶不存在回 domain.ErrAccountNotFound。
	GetByAccount(ctx context.Context, accountID uuid.UUID) (*domain.Balance, error)
	// ListByMerchant 取得商戶所有帳戶的餘額（currency 空 = 全部幣別）。
	ListByMerchant(ctx context.Context, merchantID uuid.UUID, currency string, livemode bool) ([]*domain.Balance, error)
}

// Inbox 為消費端去重（processed_events）。
type Inbox interface {
	// MarkProcessed 在目前交易內記錄 (event_id, consumer)；已存在回 already=true。
	MarkProcessed(ctx context.Context, eventID uuid.UUID, consumer string) (already bool, err error)
}

// OutboxMessage 為要寫入 outbox 的事件。
type OutboxMessage struct {
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	Headers       map[string]string
}

// OutboxStore 為 outbox 表寫入介面（與業務寫入同一交易）。
type OutboxStore interface {
	Insert(ctx context.Context, msg OutboxMessage) (eventID string, err error)
}

// Clock 提供目前時間（測試可替換）。
type Clock interface {
	Now() time.Time
}

// RealClock 以 time.Now 實作 Clock。
type RealClock struct{}

// Now 回傳 UTC 現在時間。
func (RealClock) Now() time.Time { return time.Now().UTC() }

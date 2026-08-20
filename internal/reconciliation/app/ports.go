// Package app 為 reconciliation-service 的應用層：use cases 與 ports（由 adapter 實作）。
//
// 不得 import adapter；交易邊界透過 TxManager 抽象，repo 從 ctx 取得進行中的交易。
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
)

// TxManager 提供交易邊界；fn 內透過同一個 ctx 呼叫的 repo 都在同一交易內。巢狀呼叫重用外層交易。
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// FileRepo 存取 settlement_files。
type FileRepo interface {
	// Create 新增；file_hash 重複時回傳 domain.ErrDuplicateFile。
	Create(ctx context.Context, f *domain.SettlementFile) error
	// GetByHash 以 file_hash 查詢；查無回傳 domain.ErrFileNotFound。
	GetByHash(ctx context.Context, hash string) (*domain.SettlementFile, error)
	// GetByID 以 id 查詢；查無回傳 domain.ErrFileNotFound。
	GetByID(ctx context.Context, id uuid.UUID) (*domain.SettlementFile, error)
	// Update 以樂觀鎖更新（version = f.Version，成功後 f.Version+1）；衝突回傳 domain.ErrConcurrentModification。
	Update(ctx context.Context, f *domain.SettlementFile) error
}

// LineRepo 存取 settlement_lines。
type LineRepo interface {
	// InsertBatch 批次寫入；(file_id, line_no) 重複時略過（重跑匯入冪等）。
	InsertBatch(ctx context.Context, lines []domain.SettlementLine) error
	// ListByFile 依 line_no 排序回傳某檔案所有列。
	ListByFile(ctx context.Context, fileID uuid.UUID) ([]domain.SettlementLine, error)
}

// PaymentRecordRepo 存取 payment_records 讀模型。
type PaymentRecordRepo interface {
	// Upsert 寫入 / 更新；來源 seq 比既有更舊時不套用並回傳 applied=false。
	Upsert(ctx context.Context, r *domain.PaymentRecord) (applied bool, err error)
	// Get 以 id 查詢；查無回傳 (nil, nil)。
	Get(ctx context.Context, id uuid.UUID) (*domain.PaymentRecord, error)
	// FindByProviderRefs 回傳 provider 下 provider_reference 在 refs 內的所有紀錄。
	FindByProviderRefs(ctx context.Context, provider string, refs []string) ([]domain.PaymentRecord, error)
	// ListUnsettled 回傳 provider 下「應結算」且 occurred_at < before、尚未被任何結算列對上的紀錄（本地 JOIN settlement_lines）。
	ListUnsettled(ctx context.Context, provider string, before time.Time, limit int) ([]domain.PaymentRecord, error)
}

// RunFilter 為 ListReconciliationRuns 的篩選條件。
type RunFilter struct {
	Provider string
	Statuses []domain.RunStatus
	// DateFrom / DateTo 為結算日（含），以 period_start 比對；零值不篩選。
	DateFrom time.Time
	DateTo   time.Time
	PageSize int
	// PageToken 為上一頁回傳的游標（不透明字串）。
	PageToken string
}

// RunRepo 存取 reconciliation_runs。
type RunRepo interface {
	Create(ctx context.Context, r *domain.Run) error
	// Update 以樂觀鎖更新；衝突回傳 domain.ErrConcurrentModification。
	Update(ctx context.Context, r *domain.Run) error
	// GetByID 查無回傳 domain.ErrRunNotFound。
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Run, error)
	// FindByFileID 回傳該結算檔最近一次的 run；查無回傳 (nil, nil)。
	FindByFileID(ctx context.Context, fileID uuid.UUID) (*domain.Run, error)
	// List 依 created_at DESC 分頁；回傳下一頁 token（空字串表示沒有下一頁）。
	List(ctx context.Context, f RunFilter) (runs []domain.Run, nextToken string, err error)
}

// DiscrepancyFilter 為 ListDiscrepancies 的篩選條件。
type DiscrepancyFilter struct {
	RunID         *uuid.UUID
	MerchantID    *uuid.UUID
	Provider      string
	Kinds         []domain.DiscrepancyKind
	Statuses      []domain.DiscrepancyStatus
	PaymentID     string // internal_reference（pay_…）
	CreatedAfter  time.Time
	CreatedBefore time.Time
	PageSize      int
	PageToken     string
}

// DiscrepancyRepo 存取 discrepancies。
type DiscrepancyRepo interface {
	InsertBatch(ctx context.Context, ds []domain.Discrepancy) error
	// GetByID 查無回傳 domain.ErrDiscrepancyNotFound。
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Discrepancy, error)
	// Update 以樂觀鎖更新；衝突回傳 domain.ErrConcurrentModification。
	Update(ctx context.Context, d *domain.Discrepancy) error
	// List 依 created_at DESC 分頁。
	List(ctx context.Context, f DiscrepancyFilter) (ds []domain.Discrepancy, nextToken string, err error)
	// ExistsOpen 回傳同 provider 下是否已有同 kind、同參照（provider_reference 或 internal_reference 任一相同）的 open 差異（避免跨 run 重複開單）。
	ExistsOpen(ctx context.Context, provider string, kind domain.DiscrepancyKind, providerRef, internalRef string) (bool, error)
}

// Inbox 為消費端去重（processed_events）。
type Inbox interface {
	// MarkProcessed 記錄 (event_id, consumer)；已存在回傳 already=true。必須在交易內呼叫。
	MarkProcessed(ctx context.Context, eventID, consumer string) (already bool, err error)
}

// OutboxMessage 為要寫入 outbox 的事件（對應 pkg/outbox.Message）。
type OutboxMessage struct {
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	Headers       map[string]string
}

// OutboxStore 寫入 outbox 表（必須在交易內呼叫）；回傳 event id。
type OutboxStore interface {
	Insert(ctx context.Context, msg OutboxMessage) (eventID string, err error)
}

// Clock 提供目前時間（測試可替換）。
type Clock interface {
	Now() time.Time
}

// ClockFunc 讓函式直接當 Clock 用。
type ClockFunc func() time.Time

// Now 實作 Clock。
func (f ClockFunc) Now() time.Time { return f() }

// SystemClock 回傳使用 time.Now 的 Clock。
func SystemClock() Clock { return ClockFunc(time.Now) }

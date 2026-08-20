package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
)

// Clock 提供目前時間（測試可注入固定時鐘）。
type Clock interface {
	Now() time.Time
}

// ClockFunc 讓函式直接當 Clock 使用。
type ClockFunc func() time.Time

// Now 實作 Clock。
func (f ClockFunc) Now() time.Time { return f() }

// Transactor 在單一 DB 交易內執行 fn；fn 內以同一個 ctx 呼叫的 repo 操作都落在該交易。
// 巢狀呼叫時（ctx 已在交易內）直接沿用外層交易。
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Inbox 為消費端去重（processed_events）；必須在交易內呼叫。
type Inbox interface {
	// MarkProcessed 記錄 (event_id, consumer)；已存在時回 already=true。
	MarkProcessed(ctx context.Context, eventID uuid.UUID, consumer string) (already bool, err error)
}

// EventRepo 存取 webhook_events。
type EventRepo interface {
	// Insert 寫入事件；event_id 已存在時視為成功（冪等）。
	Insert(ctx context.Context, ev *domain.Event) error
	// Get 取得事件；跨商戶或不存在時回 domain.ErrEventNotFound。
	Get(ctx context.Context, merchantID, eventID uuid.UUID) (*domain.Event, error)
}

// DeliveryFilter 為 ListDeliveries 的篩選條件。
type DeliveryFilter struct {
	MerchantID    uuid.UUID
	EndpointID    *uuid.UUID
	EventID       *uuid.UUID
	EventType     string
	Statuses      []domain.DeliveryStatus
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	Livemode      *bool
	PageSize      int
	PageToken     string
}

// DeliveryPage 為分頁結果。
type DeliveryPage struct {
	Deliveries    []*domain.Delivery
	NextPageToken string
}

// DeliveryRepo 存取 webhook_deliveries / webhook_delivery_attempts。
type DeliveryRepo interface {
	// InsertPending 批次寫入 pending deliveries；(event_id, endpoint_id) 已存在時略過。
	InsertPending(ctx context.Context, ds []*domain.Delivery) error
	// ClaimDue 取件：pending/failed 且 next_attempt_at <= now 的列改為 in_flight、attempt_no+1（FOR UPDATE SKIP LOCKED），
	// 並帶出事件投影欄位（EventType / EventPayload / Livemode）。
	ClaimDue(ctx context.Context, now time.Time, limit int) ([]*domain.Delivery, error)
	// Save 以樂觀鎖（version）寫回 delivery 狀態，att 非 nil 時同交易寫入一筆 attempt。
	Save(ctx context.Context, d *domain.Delivery, att *domain.Attempt) error
	// Get 取得 delivery（含事件投影欄位）；跨商戶或不存在回 domain.ErrDeliveryNotFound。
	Get(ctx context.Context, merchantID, id uuid.UUID) (*domain.Delivery, error)
	// ListAttempts 依 attempt_no 升冪列出所有嘗試。
	ListAttempts(ctx context.Context, deliveryID uuid.UUID) ([]*domain.Attempt, error)
	// List 依條件分頁列出（created_at DESC）。
	List(ctx context.Context, f DeliveryFilter) (*DeliveryPage, error)
	// ReapStuck 把 updated_at < before 的 in_flight 列轉回 failed（立即可重送），回傳筆數。
	ReapStuck(ctx context.Context, before, now time.Time) (int64, error)
	// CancelForEndpoint 取消端點所有尚未成功的 delivery，回傳筆數。
	CancelForEndpoint(ctx context.Context, endpointID uuid.UUID, now time.Time, reason string) (int64, error)
}

// EndpointSource 提供商戶端點（含簽章 secret）。實作：gRPC 到 merchant-service（外層包 TTL 快取）。
type EndpointSource interface {
	// ListEndpoints 列出商戶所有未刪除端點（含 disabled，由 domain 過濾）。
	ListEndpoints(ctx context.Context, merchantID uuid.UUID) ([]*domain.Endpoint, error)
	// GetEndpoint 取得單一端點；不存在時回 (nil, nil)。
	GetEndpoint(ctx context.Context, merchantID, endpointID uuid.UUID) (*domain.Endpoint, error)
}

// EndpointInvalidator 為可選介面：快取層實作，讓端點被停用後立即失效。
type EndpointInvalidator interface {
	Invalidate(merchantID uuid.UUID)
}

// EndpointDisabler 回報端點應停用（410 Gone）；實作為 merchant-service UpdateWebhookEndpoint(status=DISABLED)。
type EndpointDisabler interface {
	DisableEndpoint(ctx context.Context, merchantID, endpointID uuid.UUID, reason string) error
}

// SendRequest 為一次 HTTP 投遞的輸入（header 已由 use case 備妥，含簽章）。
type SendRequest struct {
	URL     string
	Body    []byte
	Headers map[string]string
}

// HTTPSender 送出 webhook；不回 error，所有失敗都表達在 Outcome（Err / StatusCode）。
type HTTPSender interface {
	Send(ctx context.Context, req SendRequest) domain.Outcome
}

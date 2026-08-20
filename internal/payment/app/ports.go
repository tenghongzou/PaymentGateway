// Package app 為 payment-service 的應用層：use cases 與 ports（介面）。
// 只依賴 domain、pkg/ 與 api/gen（proto 型別）；不得 import adapter。
package app

import (
	"context"
	"errors"
	"time"

	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/money"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
)

// ErrDuplicateIdempotencyKey 由 repo 在 (merchant_id, idempotency_key) 唯一索引衝突時回傳。
var ErrDuplicateIdempotencyKey = errors.New("payment: duplicate idempotency key")

// Clock 抽象時間來源（測試可固定）。
type Clock interface {
	Now() time.Time
}

// RealClock 以 time.Now().UTC() 實作 Clock。
type RealClock struct{}

// Now 回傳目前 UTC 時間。
func (RealClock) Now() time.Time { return time.Now().UTC() }

// TxManager 在一個資料庫交易內執行 fn；repo / outbox 透過 ctx 取得交易（adapter 負責放入）。
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// ListFilter 為 ListPayments 的篩選條件。
type ListFilter struct {
	Statuses      []domain.Status
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	CustomerID    string
	Provider      string
	Currency      string
	Limit         int
	Cursor        string
}

// PaymentRepo 為 Payment / Attempt / Refund 的持久化介面。
// 標示「tx」的方法必須在 TxManager.WithinTx 內呼叫；其餘可用 pool 直接查詢。
type PaymentRepo interface {
	// CreatePayment（tx）INSERT payments；(merchant_id, idempotency_key) 衝突回 ErrDuplicateIdempotencyKey。
	CreatePayment(ctx context.Context, p *domain.Payment) error
	// GetPayment 依公開 ID 讀取（含 attempts）；找不到回 domain.ErrPaymentNotFound。
	GetPayment(ctx context.Context, merchantID, publicID string) (*domain.Payment, error)
	// GetPaymentByIdempotencyKey 依冪等鍵讀取；找不到回 domain.ErrPaymentNotFound。
	GetPaymentByIdempotencyKey(ctx context.Context, merchantID, key string) (*domain.Payment, error)
	// GetPaymentForUpdate（tx）SELECT ... FOR UPDATE。
	GetPaymentForUpdate(ctx context.Context, merchantID, publicID string) (*domain.Payment, error)
	// UpdatePayment（tx）UPDATE ... WHERE version = expectedVersion；0 列回 pgdb.ErrConcurrentModification。
	UpdatePayment(ctx context.Context, p *domain.Payment, expectedVersion int) error
	// InsertAttempt / UpdateAttempt（tx）。
	InsertAttempt(ctx context.Context, a *domain.Attempt) error
	UpdateAttempt(ctx context.Context, a *domain.Attempt) error
	// AppendEvents（tx）寫入 payment_events（seq = event.Seq）。
	AppendEvents(ctx context.Context, p *domain.Payment, events []domain.Event, traceID string) error
	// ListPayments 依 created_at DESC, id DESC 分頁。
	ListPayments(ctx context.Context, merchantID string, f ListFilter) (items []*domain.Payment, nextCursor string, err error)

	// CreateRefund（tx）INSERT refunds；唯一鍵衝突回 ErrDuplicateIdempotencyKey。
	CreateRefund(ctx context.Context, r *domain.Refund) error
	GetRefund(ctx context.Context, merchantID, publicID string) (*domain.Refund, error)
	GetRefundByIdempotencyKey(ctx context.Context, merchantID, key string) (*domain.Refund, error)
	// UpdateRefund（tx）樂觀鎖更新。
	UpdateRefund(ctx context.Context, r *domain.Refund, expectedVersion int) error
}

// OutboxStore 在交易內寫入 outbox 表（需透過 TxManager.WithinTx 呼叫）。
type OutboxStore interface {
	Insert(ctx context.Context, msg outbox.Message) error
}

// ProviderClient 為對單一 PSP adapter 的呼叫介面（與 pg.provider.v1.ProviderAdapter 對應，不帶 CallOption）。
type ProviderClient interface {
	Authorize(ctx context.Context, req *providerv1.AuthorizeRequest) (*providerv1.AuthorizeResponse, error)
	Capture(ctx context.Context, req *providerv1.CaptureRequest) (*providerv1.CaptureResponse, error)
	Void(ctx context.Context, req *providerv1.VoidRequest) (*providerv1.VoidResponse, error)
	Refund(ctx context.Context, req *providerv1.RefundRequest) (*providerv1.RefundResponse, error)
	GetPaymentStatus(ctx context.Context, req *providerv1.GetPaymentStatusRequest) (*providerv1.GetPaymentStatusResponse, error)
}

// ProviderRegistry 依名稱取得 ProviderClient。
type ProviderRegistry interface {
	Get(name string) (ProviderClient, bool)
	Names() []string
}

// RoutingContext 為路由輸入（docs/02 §9.1 子集）。
type RoutingContext struct {
	MerchantID        string
	Amount            money.Money
	PaymentMethodType string
	// TokenProvider 非空時付款鎖定在該 Provider（token 不可攜）。
	TokenProvider     string
	CaptureMethod     domain.CaptureMethod
	PreferredProvider string
	LiveMode          bool
}

// Candidate 為有序候選。
type Candidate struct {
	Provider string
	Reason   string // token_provider | preferred | default | fallback
}

// Router 產生有序候選並接收結果回報（circuit breaker 用）。
type Router interface {
	Route(ctx context.Context, rc RoutingContext) ([]Candidate, error)
	Report(provider string, cat domain.ProviderErrorCategory)
}

package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// APIVersion 為 webhook payload 的 api_version（OpenAPI Event.api_version const: v1）。
const APIVersion = "v1"

// 資源類型（webhook_events.resource_type）。
const (
	ResourcePayment = "payment"
	ResourceRefund  = "refund"
	ResourceDispute = "dispute"
)

// Event 為要通知商戶的事件（webhook_events 一列）。Payload 為已序列化的對外 JSON（OpenAPI Event 物件），
// 投遞時以原始位元組簽章，不再重新序列化。
type Event struct {
	// ID 為來源事件 id（evt_ 的 uuid 部分），同時是去重鍵。
	ID           uuid.UUID
	MerchantID   uuid.UUID
	Type         string // payment.captured / refund.succeeded ...
	ResourceType string // payment / refund / dispute
	ResourceID   string // pay_ / re_ / dp_
	PaymentID    string
	Livemode     bool
	Payload      []byte
	OccurredAt   time.Time
	CreatedAt    time.Time
}

// PublicID 回傳 evt_ 形式的事件 ID（X-PG-Event-Id）。
func (e *Event) PublicID() string { return EventPublicID(e.ID) }

// EventTypeInfo 為事件類型描述（ListEventTypes）。
type EventTypeInfo struct {
	Name        string
	Description string
	ObjectType  string
	Terminal    bool
}

// EventTypes 為系統支援的所有 webhook 事件類型（docs/03 §5.1）。
var EventTypes = []EventTypeInfo{
	{Name: "payment.created", Description: "付款已建立（尚未授權）。", ObjectType: ResourcePayment},
	{Name: "payment.requires_action", Description: "需要客戶完成 3DS / 導向。", ObjectType: ResourcePayment},
	{Name: "payment.authorized", Description: "授權成功，待請款（僅 manual capture）。", ObjectType: ResourcePayment},
	{Name: "payment.captured", Description: "請款成功。", ObjectType: ResourcePayment},
	{Name: "payment.voided", Description: "授權已取消。", ObjectType: ResourcePayment, Terminal: true},
	{Name: "payment.failed", Description: "付款失敗（授權 / 3DS 失敗）。", ObjectType: ResourcePayment, Terminal: true},
	{Name: "payment.expired", Description: "付款逾期（3DS 逾時或授權逾期）。", ObjectType: ResourcePayment, Terminal: true},
	{Name: "refund.created", Description: "退款已建立並送交 PSP。", ObjectType: ResourceRefund},
	{Name: "refund.succeeded", Description: "退款成功。", ObjectType: ResourceRefund, Terminal: true},
	{Name: "refund.failed", Description: "退款失敗。", ObjectType: ResourceRefund, Terminal: true},
	{Name: "dispute.opened", Description: "PSP 通知爭議開啟。", ObjectType: ResourceDispute},
	{Name: "dispute.evidence_submitted", Description: "爭議證據已送交 PSP。", ObjectType: ResourceDispute},
	{Name: "dispute.won", Description: "爭議商戶勝訴。", ObjectType: ResourceDispute, Terminal: true},
	{Name: "dispute.lost", Description: "爭議商戶敗訴。", ObjectType: ResourceDispute, Terminal: true},
}

// LookupEventType 依名稱查事件類型。
func LookupEventType(name string) (EventTypeInfo, bool) {
	for _, t := range EventTypes {
		if t.Name == name {
			return t, true
		}
	}
	return EventTypeInfo{}, false
}

// IsKnownEventType 檢查事件名稱是否在支援清單內。
func IsKnownEventType(name string) bool {
	_, ok := LookupEventType(name)
	return ok
}

// EventTypeFromEnumName 把 protobuf enum 名稱轉成 webhook 事件名稱：
// 去掉 PAYMENT_EVENT_TYPE_ 前綴、小寫、第一個 "_" 轉 "."（events.proto 規則）。
// 例：PAYMENT_EVENT_TYPE_PAYMENT_REQUIRES_ACTION → payment.requires_action。
func EventTypeFromEnumName(enumName string) string {
	s := strings.ToLower(strings.TrimPrefix(enumName, "PAYMENT_EVENT_TYPE_"))
	if s == "" || s == "unspecified" {
		return ""
	}
	return strings.Replace(s, "_", ".", 1)
}

// ---------------------------------------------------------------------------
// 對外 JSON 形狀（api/openapi/payment-gateway.yaml：Event / Payment / Refund / Dispute / Money）
// ---------------------------------------------------------------------------

// MoneyJSON 對應 OpenAPI Money。
type MoneyJSON struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// RequestInfoJSON 對應 Event.request（由 PSP webhook 觸發時為 null）。
type RequestInfoJSON struct {
	ID             string  `json:"id"`
	IdempotencyKey *string `json:"idempotency_key"`
}

// EventJSON 對應 OpenAPI Event（webhook body 與 GET /events/{id} 相同）。
type EventJSON struct {
	ID         string           `json:"id"`
	Object     string           `json:"object"`
	Type       string           `json:"type"`
	APIVersion string           `json:"api_version"`
	Livemode   bool             `json:"livemode"`
	CreatedAt  time.Time        `json:"created_at"`
	PaymentID  string           `json:"payment_id,omitempty"`
	Request    *RequestInfoJSON `json:"request"`
	Data       EventDataJSON    `json:"data"`
}

// EventDataJSON 對應 Event.data。
type EventDataJSON struct {
	Object any `json:"object"`
}

// PaymentMethodJSON 對應 OpenAPI PaymentMethodDetails。
type PaymentMethodJSON struct {
	Type          string `json:"type"`
	Brand         string `json:"brand,omitempty"`
	Last4         string `json:"last4,omitempty"`
	Issuer        string `json:"issuer,omitempty"`
	IssuerCountry string `json:"issuer_country,omitempty"`
	Funding       string `json:"funding,omitempty"`
	ThreeDSResult string `json:"three_ds_result,omitempty"`
}

// ErrorDetailJSON 對應 OpenAPI ErrorDetail（Payment.last_error）。
type ErrorDetailJSON struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NextActionJSON 對應 OpenAPI NextAction（僅 payment.requires_action 帶核心欄位）。
type NextActionJSON struct {
	Type      string     `json:"type"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// CustomerJSON 對應 OpenAPI Customer（目前只帶 id）。
type CustomerJSON struct {
	ID string `json:"id"`
}

// PaymentJSON 對應 OpenAPI Payment 的核心欄位（事件 payload 自帶的資訊；未知欄位省略）。
type PaymentJSON struct {
	ID                     string             `json:"id"`
	Object                 string             `json:"object"`
	Status                 string             `json:"status"`
	Version                int64              `json:"version"`
	Amount                 *MoneyJSON         `json:"amount,omitempty"`
	CapturedAmount         *MoneyJSON         `json:"captured_amount,omitempty"`
	RefundedAmount         *MoneyJSON         `json:"refunded_amount,omitempty"`
	Fee                    *MoneyJSON         `json:"fee,omitempty"`
	CaptureMethod          string             `json:"capture_method,omitempty"`
	PaymentMethod          *PaymentMethodJSON `json:"payment_method,omitempty"`
	Customer               *CustomerJSON      `json:"customer,omitempty"`
	Metadata               map[string]string  `json:"metadata"`
	Description            string             `json:"description,omitempty"`
	Provider               string             `json:"provider,omitempty"`
	ProviderReference      string             `json:"provider_reference,omitempty"`
	NextAction             *NextActionJSON    `json:"next_action,omitempty"`
	LastError              *ErrorDetailJSON   `json:"last_error,omitempty"`
	IsFinalCapture         *bool              `json:"is_final_capture,omitempty"`
	VoidReason             string             `json:"void_reason,omitempty"`
	PreviousStatus         string             `json:"previous_status,omitempty"`
	Livemode               bool               `json:"livemode"`
	AuthorizationExpiresAt *time.Time         `json:"expires_at,omitempty"`
	UpdatedAt              time.Time          `json:"updated_at"`
}

// RefundJSON 對應 OpenAPI Refund 的核心欄位。
type RefundJSON struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	PaymentID         string            `json:"payment_id"`
	Amount            *MoneyJSON        `json:"amount,omitempty"`
	Status            string            `json:"status"`
	Reason            string            `json:"reason,omitempty"`
	Provider          string            `json:"provider,omitempty"`
	ProviderReference string            `json:"provider_reference,omitempty"`
	Fee               *MoneyJSON        `json:"fee,omitempty"`
	FailureCode       string            `json:"failure_code,omitempty"`
	FailureMessage    string            `json:"failure_message,omitempty"`
	Metadata          map[string]string `json:"metadata"`
	Livemode          bool              `json:"livemode"`
	UpdatedAt         time.Time         `json:"updated_at"`
	CompletedAt       *time.Time        `json:"completed_at,omitempty"`
}

// DisputeJSON 對應 OpenAPI Dispute 的核心欄位。
type DisputeJSON struct {
	ID                  string     `json:"id"`
	Object              string     `json:"object"`
	PaymentID           string     `json:"payment_id"`
	Amount              *MoneyJSON `json:"amount,omitempty"`
	Fee                 *MoneyJSON `json:"fee,omitempty"`
	Status              string     `json:"status"`
	Reason              string     `json:"reason,omitempty"`
	Provider            string     `json:"provider,omitempty"`
	ProviderReference   string     `json:"provider_reference,omitempty"`
	EvidenceDueBy       *time.Time `json:"evidence_due_by,omitempty"`
	EvidenceSubmittedAt *time.Time `json:"evidence_submitted_at,omitempty"`
	ClosedAt            *time.Time `json:"closed_at,omitempty"`
	Livemode            bool       `json:"livemode"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

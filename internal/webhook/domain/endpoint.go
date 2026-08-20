package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// EndpointStatus 對應 merchant-service 的 WebhookEndpointStatus。
type EndpointStatus string

// 端點狀態。
const (
	EndpointEnabled      EndpointStatus = "enabled"
	EndpointDisabled     EndpointStatus = "disabled"
	EndpointAutoDisabled EndpointStatus = "auto_disabled"
	EndpointDeleted      EndpointStatus = "deleted"
)

// Endpoint 為商戶的 webhook 端點（來源：merchant-service ListWebhookEndpoints(include_secrets=true)）。
type Endpoint struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	URL        string
	// Secrets 為簽章用 secret，index 0 為 current，其後為輪替中仍有效的 previous；投遞時每把各產生一個 v1。
	Secrets []string
	// EnabledEvents 為訂閱清單；空或含 "*" 表示全部，支援 "payment.*" 字首萬用字元。
	EnabledEvents []string
	Status        EndpointStatus
	// Livemode 為端點接收 live（true）或 test（false）事件。
	Livemode bool
}

// PublicID 回傳 we_ 形式。
func (e *Endpoint) PublicID() string { return EndpointPublicID(e.ID) }

// Enabled 回傳端點是否啟用。
func (e *Endpoint) Enabled() bool { return e != nil && e.Status == EndpointEnabled }

// Subscribes 回傳端點是否訂閱此事件類型。
func (e *Endpoint) Subscribes(eventType string) bool {
	if len(e.EnabledEvents) == 0 {
		return true
	}
	for _, pat := range e.EnabledEvents {
		pat = strings.TrimSpace(pat)
		switch {
		case pat == "*":
			return true
		case pat == eventType:
			return true
		case strings.HasSuffix(pat, ".*") && strings.HasPrefix(eventType, strings.TrimSuffix(pat, "*")):
			return true
		}
	}
	return false
}

// Accepts 回傳此端點是否應收到該事件：啟用中、live/test 模式相符且有訂閱。
func (e *Endpoint) Accepts(ev *Event) bool {
	return e.Enabled() && e.Livemode == ev.Livemode && e.Subscribes(ev.Type)
}

// ActiveSecrets 回傳非空的 secret 清單（保持順序）。
func (e *Endpoint) ActiveSecrets() []string {
	out := make([]string, 0, len(e.Secrets))
	for _, s := range e.Secrets {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// FanOut 為事件挑出應投遞的端點並建立 pending deliveries。
func FanOut(ev *Event, endpoints []*Endpoint, now func() time.Time) []*Delivery {
	var out []*Delivery
	for _, ep := range endpoints {
		if ep.Accepts(ev) {
			out = append(out, NewDelivery(ev, ep, now()))
		}
	}
	return out
}

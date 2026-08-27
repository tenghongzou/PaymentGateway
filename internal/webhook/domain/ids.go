package domain

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// 對外 ID 與資料庫 uuid 的雙向轉換。
//
// pkg/ids 的公開 ID 是 prefix + "_" + base32(UUIDv7)，因此 evt_/mch_/we_/whd_ 都能無損對應到 uuid 欄位；
// 為了相容 Kafka header 直接帶 outbox.id（純 uuid）的情況，解析時也接受 uuid 字串。

// ParseID 解析公開 ID（prefix 必須相符）或純 uuid 字串。
func ParseID(s, prefix string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, fmt.Errorf("%w: empty %s id", ErrInvalidID, prefix)
	}
	if u, err := uuid.Parse(s); err == nil {
		return u, nil
	}
	u, err := ids.ParseWithPrefix(s, prefix)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %w", ErrInvalidID, err)
	}
	return u, nil
}

// ParseEventID 解析 evt_ 事件 ID。
func ParseEventID(s string) (uuid.UUID, error) { return ParseID(s, ids.PrefixEvent) }

// ParseMerchantID 解析 mch_ 商戶 ID。
func ParseMerchantID(s string) (uuid.UUID, error) { return ParseID(s, ids.PrefixMerchant) }

// ParseEndpointID 解析 we_ 端點 ID。
func ParseEndpointID(s string) (uuid.UUID, error) { return ParseID(s, ids.PrefixWebhookEndpoint) }

// ParseDeliveryID 解析 whd_ delivery ID。
func ParseDeliveryID(s string) (uuid.UUID, error) { return ParseID(s, ids.PrefixWebhookDelivery) }

// EventPublicID 回傳 evt_ 形式。
func EventPublicID(u uuid.UUID) string { return ids.Format(ids.PrefixEvent, u) }

// MerchantPublicID 回傳 mch_ 形式。
func MerchantPublicID(u uuid.UUID) string { return ids.Format(ids.PrefixMerchant, u) }

// EndpointPublicID 回傳 we_ 形式。
func EndpointPublicID(u uuid.UUID) string { return ids.Format(ids.PrefixWebhookEndpoint, u) }

// DeliveryPublicID 回傳 whd_ 形式。
func DeliveryPublicID(u uuid.UUID) string { return ids.Format(ids.PrefixWebhookDelivery, u) }

// Package ids 產生與解析對外公開 ID。
//
// 格式：<prefix>_<26 碼 Crockford base32>，內容為 UUIDv7 的 128 bits（時間可排序，與 ULID 字元集相同）。
// 對應 docs/02 §0.2；前綴以 migrations 的 CHECK 約束為準（refund 為 re_、dispute 為 dp_、webhook endpoint 為 we_）。
package ids

import (
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// 公開 ID 前綴（不含底線）。
const (
	PrefixPayment         = "pay"
	PrefixAttempt         = "att"
	PrefixRefund          = "re" // migrations/payment: refunds.public_id ~ '^re_'
	PrefixDispute         = "dp" // migrations/payment: disputes.public_id ~ '^dp_'
	PrefixMerchant        = "mch"
	PrefixAPIKey          = "key"
	PrefixWebhookEndpoint = "we" // migrations/merchant: webhook_endpoints.public_id ~ '^we_'
	PrefixWebhookDelivery = "whd"
	PrefixEvent           = "evt"
	PrefixJournal         = "jrn"
	PrefixEntry           = "ent"
	PrefixAccount         = "acct"
	PrefixSettlement      = "stl"
	PrefixRequest         = "req"
)

// ErrInvalid 表示 ID 格式不正確。
var ErrInvalid = errors.New("ids: invalid id")

// crockford 為 Crockford base32 字元集（無 I L O U），不補 padding。
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// encodedLen 為 16 bytes 的 base32 長度（26）。
const encodedLen = 26

// NewUUID 產生 UUIDv7（給資料庫 uuid 欄位）。
func NewUUID() uuid.UUID {
	u, err := uuid.NewV7()
	if err != nil {
		// uuid.NewV7 只在亂數來源失效時出錯，屬不可恢復狀況。
		panic(fmt.Sprintf("ids: uuid v7: %v", err))
	}
	return u
}

// New 產生新的公開 ID：prefix + "_" + base32(UUIDv7)。長度固定為 len(prefix)+1+26。
func New(prefix string) string {
	return Format(prefix, NewUUID())
}

// Format 以既有的 UUID 組出公開 ID（同一個 uuid 永遠對應同一個公開 ID，可從 DB 主鍵推導）。
func Format(prefix string, u uuid.UUID) string {
	return prefix + "_" + crockford.EncodeToString(u[:])
}

// Parse 解析公開 ID，回傳前綴與 UUID。
func Parse(id string) (prefix string, u uuid.UUID, err error) {
	i := strings.LastIndexByte(id, '_')
	if i <= 0 || i == len(id)-1 {
		return "", uuid.Nil, fmt.Errorf("%w: %q", ErrInvalid, id)
	}
	prefix, body := id[:i], id[i+1:]
	if len(body) != encodedLen {
		return "", uuid.Nil, fmt.Errorf("%w: %q", ErrInvalid, id)
	}
	raw, err := crockford.DecodeString(body)
	if err != nil || len(raw) != 16 {
		return "", uuid.Nil, fmt.Errorf("%w: %q", ErrInvalid, id)
	}
	copy(u[:], raw)
	return prefix, u, nil
}

// ParseWithPrefix 解析並檢查前綴是否符合預期。
func ParseWithPrefix(id, wantPrefix string) (uuid.UUID, error) {
	p, u, err := Parse(id)
	if err != nil {
		return uuid.Nil, err
	}
	if p != wantPrefix {
		return uuid.Nil, fmt.Errorf("%w: expected prefix %q, got %q", ErrInvalid, wantPrefix, p)
	}
	return u, nil
}

// HasPrefix 檢查 ID 是否以指定前綴開頭（不驗證內容）。
func HasPrefix(id, prefix string) bool {
	return strings.HasPrefix(id, prefix+"_")
}

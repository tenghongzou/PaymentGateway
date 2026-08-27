package postgres

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// cursor 為 keyset 分頁游標（docs/03 §3.1：排序鍵 (created_at, id) + 篩選條件 hash）。
type cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        uuid.UUID `json:"i"`
	Filter    string    `json:"f"`
}

// filterHash 把篩選條件摘要成短字串，沿用舊 cursor 但改篩選條件時拒絕。
func filterHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p)) // hash.Hash.Write 不回錯誤
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

func encodeCursor(createdAt time.Time, id uuid.UUID, filter string) string {
	b, err := json.Marshal(cursor{CreatedAt: createdAt, ID: id, Filter: filter})
	if err != nil {
		return "" // cursor 欄位皆可序列化，理論上不會發生；發生時視為無下一頁
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor 解析 token；空 token 回 nil。
func decodeCursor(token, filter string) (*cursor, error) {
	if token == "" {
		return nil, nil //nolint:nilnil // 空 token = 第一頁
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, domain.ErrParameterInvalid.WithParam("page_token").WithMessage("page_token is malformed")
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == uuid.Nil {
		return nil, domain.ErrParameterInvalid.WithParam("page_token").WithMessage("page_token is malformed")
	}
	if c.Filter != filter {
		return nil, domain.ErrParameterInvalid.WithParam("page_token").WithMessage("page_token does not match the current filter")
	}
	return &c, nil
}

// query 為簡單的 WHERE 組裝器。
type query struct {
	where []string
	args  []any
}

func (q *query) add(cond string, args ...any) {
	q.where = append(q.where, cond)
	q.args = append(q.args, args...)
}

// next 回傳下一個 placeholder 編號。
func (q *query) next() int { return len(q.args) + 1 }

func (q *query) sql() string {
	if len(q.where) == 0 {
		return ""
	}
	out := " WHERE " + q.where[0]
	for _, w := range q.where[1:] {
		out += " AND " + w
	}
	return out
}

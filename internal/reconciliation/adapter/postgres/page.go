package postgres

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
)

// cursor 為 keyset 分頁游標：(created_at DESC, id DESC)。
type cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// encodeCursor 把游標編成不透明 token。
func encodeCursor(c cursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor 解析 token；格式錯誤回 INVALID_ARGUMENT 等級的 apperr。
func decodeCursor(token string) (*cursor, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, apperr.ErrParameterInvalid.WithMessage("invalid page_token").WithParam("page.page_token")
	}
	ts, idStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil, apperr.ErrParameterInvalid.WithMessage("invalid page_token").WithParam("page.page_token")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, apperr.ErrParameterInvalid.WithMessage("invalid page_token").WithParam("page.page_token")
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, apperr.ErrParameterInvalid.WithMessage("invalid page_token").WithParam("page.page_token")
	}
	return &cursor{CreatedAt: t, ID: id}, nil
}

// whereBuilder 組動態 WHERE 子句。
type whereBuilder struct {
	conds []string
	args  []any
}

func (w *whereBuilder) add(cond string, args ...any) {
	// 以 $N 佔位：cond 內用 %d 表示下一個參數編號。
	idx := make([]any, len(args))
	for i := range args {
		w.args = append(w.args, args[i])
		idx[i] = len(w.args)
	}
	w.conds = append(w.conds, fmt.Sprintf(cond, idx...))
}

func (w *whereBuilder) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// next 回傳下一個參數編號（供 LIMIT 等使用）。
func (w *whereBuilder) next(arg any) int {
	w.args = append(w.args, arg)
	return len(w.args)
}

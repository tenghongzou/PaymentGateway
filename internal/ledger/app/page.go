package app

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
)

// 分頁預設值（docs/03 / pagination.proto）。
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// ErrPageTokenInvalid 為 page_token 解析失敗。
var ErrPageTokenInvalid = apperr.New(apperr.TypeInvalidRequest, "parameter_invalid", "page_token is invalid.").WithParam("page.page_token")

// NewPage 把 (page_size, page_token) 正規化為 Page。
func NewPage(size int, token string) (Page, error) {
	if size <= 0 {
		size = DefaultPageSize
	}
	if size > MaxPageSize {
		size = MaxPageSize
	}
	p := Page{Limit: size}
	if token == "" {
		return p, nil
	}
	c, err := DecodeCursor(token)
	if err != nil {
		return Page{}, err
	}
	p.After = &c
	return p, nil
}

// EncodeCursor 把游標編成不透明 token（base64url(unix_nano|uuid)）。
func EncodeCursor(c *Cursor) string {
	if c == nil {
		return ""
	}
	raw := strconv.FormatInt(c.At.UnixNano(), 10) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor 解析 token。
func DecodeCursor(token string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, ErrPageTokenInvalid.Wrap(err)
	}
	tsStr, idStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return Cursor{}, ErrPageTokenInvalid
	}
	ns, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return Cursor{}, ErrPageTokenInvalid.Wrap(err)
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Cursor{}, ErrPageTokenInvalid.Wrap(fmt.Errorf("cursor id: %w", err))
	}
	return Cursor{At: time.Unix(0, ns).UTC(), ID: id}, nil
}

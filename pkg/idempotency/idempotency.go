// Package idempotency 實作 api-gateway 的 Idempotency-Key 儲存（docs/02 §8.1、docs/05 §10）。
//
// 流程：Begin 以 SETNX 取得鎖並記錄 request_hash；處理完成後 Complete 寫入快取回應（TTL 24h）；
// 上游 5xx 時 Abort 刪除鍵讓商戶可重試。
package idempotency

import (
	"context"
	"errors"
	"time"
)

// 錯誤。
var (
	// ErrMismatch 表示同 key 但 request hash 不同。
	ErrMismatch = errors.New("idempotency: key reused with different payload")
	// ErrInProgress 表示同 key 的前一個請求仍在處理中。
	ErrInProgress = errors.New("idempotency: request in progress")
)

// State 為 Begin 的結果狀態。
type State int

// 狀態列舉。
const (
	// StateNew 表示取得鎖，呼叫端應繼續處理並於結束時 Complete / Abort。
	StateNew State = iota
	// StateCompleted 表示已有快取回應，呼叫端應直接回放。
	StateCompleted
)

// Response 為快取的 HTTP 回應。
type Response struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       []byte            `json:"body"`
}

// Store 為冪等鍵儲存介面。
type Store interface {
	// Begin 嘗試開始處理：回傳 StateNew（取得鎖）或 StateCompleted（附快取回應）；
	// 同 key 不同 hash 回 ErrMismatch，處理中回 ErrInProgress。
	Begin(ctx context.Context, merchantID, key, requestHash string) (State, *Response, error)
	// Complete 記錄最終回應（TTL 由實作決定，預設 24h）。
	Complete(ctx context.Context, merchantID, key string, resp Response) error
	// Abort 釋放鎖（上游 5xx / 逾時時使用，不快取結果）。
	Abort(ctx context.Context, merchantID, key string) error
}

// 預設 TTL。
const (
	DefaultTTL     = 24 * time.Hour
	DefaultLockTTL = 30 * time.Second
)

// record 為儲存的內容。
type record struct {
	State       string    `json:"state"` // in_progress / completed
	RequestHash string    `json:"request_hash"`
	Response    *Response `json:"response,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

const (
	stateInProgress = "in_progress"
	stateCompleted  = "completed"
)

func storeKey(merchantID, key string) string {
	return "idem:" + merchantID + ":" + key
}

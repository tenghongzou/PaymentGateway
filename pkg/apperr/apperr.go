// Package apperr 定義跨服務共用的業務錯誤型別。
//
// 所有領域錯誤都以 *Error 表達，帶有 docs/01-architecture.md §8 規定的 type / code / param；
// pkg/grpcx 把它轉成 gRPC status（夾帶 pg.common.v1.ErrorDetail），pkg/httpx 再轉回 REST 錯誤。
// HTTP 狀態碼由 (type, code) 查表決定（docs/03-api.md §7），未知 code 則依 type 給預設值。
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// 錯誤大類（docs/01-architecture.md §8）。
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeAuthentication = "authentication_error"
	TypeIdempotency    = "idempotency_error"
	TypeRateLimit      = "rate_limit_error"
	TypeProvider       = "provider_error"
	TypeAPI            = "api_error"
)

// Error 為跨服務統一的業務錯誤。
type Error struct {
	Type    string
	Code    string
	Message string
	Param   string
	// Err 為底層原因（可為 nil），不會對外揭露。
	Err error
}

// Error 實作 error。
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s/%s: %s: %v", e.Type, e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s/%s: %s", e.Type, e.Code, e.Message)
}

// Unwrap 讓 errors.Is / errors.As 能穿透到底層原因。
func (e *Error) Unwrap() error { return e.Err }

// Is 讓 errors.Is(err, target) 以 (Type, Code) 比對，方便測試與 use case 判斷。
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Type == t.Type && e.Code == t.Code
}

// HTTPStatus 回傳此錯誤對應的 HTTP 狀態碼。
func (e *Error) HTTPStatus() int { return HTTPStatus(e.Type, e.Code) }

// WithParam 回傳帶有 param 的複本。
func (e *Error) WithParam(param string) *Error {
	c := *e
	c.Param = param
	return &c
}

// WithMessage 回傳覆寫 message 的複本。
func (e *Error) WithMessage(format string, args ...any) *Error {
	c := *e
	c.Message = fmt.Sprintf(format, args...)
	return &c
}

// Wrap 回傳附帶底層原因的複本。
func (e *Error) Wrap(err error) *Error {
	c := *e
	c.Err = err
	return &c
}

// New 建立錯誤。
func New(typ, code, message string) *Error {
	return &Error{Type: typ, Code: code, Message: message}
}

// From 嘗試把任意 error 轉成 *Error；失敗回傳 nil。
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// codeStatus 為 (code → HTTP status) 對照表（docs/03-api.md §7 與 docs/02 §10 的聯集）。
var codeStatus = map[string]int{
	// invalid_request_error
	"parameter_missing":                 http.StatusBadRequest,
	"parameter_invalid":                 http.StatusBadRequest,
	"parameter_unknown":                 http.StatusBadRequest,
	"resource_missing":                  http.StatusNotFound,
	"resource_already_exists":           http.StatusConflict,
	"invalid_state_transition":          http.StatusConflict,
	"amount_too_small":                  http.StatusUnprocessableEntity,
	"amount_too_large":                  http.StatusUnprocessableEntity,
	"invalid_currency":                  http.StatusUnprocessableEntity,
	"currency_not_supported":            http.StatusUnprocessableEntity,
	"currency_mismatch":                 http.StatusUnprocessableEntity,
	"capture_amount_exceeds_authorized": http.StatusUnprocessableEntity,
	"refund_amount_exceeds_captured":    http.StatusUnprocessableEntity,
	"partial_capture_not_supported":     http.StatusUnprocessableEntity,
	"payment_method_invalid":            http.StatusUnprocessableEntity,
	"pan_not_allowed":                   http.StatusBadRequest,
	"return_url_required":               http.StatusUnprocessableEntity,
	"evidence_window_closed":            http.StatusUnprocessableEntity,
	"webhook_url_invalid":               http.StatusUnprocessableEntity,
	"webhook_endpoint_limit_reached":    http.StatusUnprocessableEntity,
	"api_key_limit_reached":             http.StatusUnprocessableEntity,
	"cannot_revoke_current_key":         http.StatusConflict,
	"webhook_signature_invalid":         http.StatusBadRequest,
	"endpoint_sunset":                   http.StatusGone,
	"payment_not_refundable":            http.StatusConflict,
	"payment_disputed":                  http.StatusConflict,
	"payment_expired":                   http.StatusConflict,
	"void_not_allowed":                  http.StatusConflict,
	"operation_in_progress":             http.StatusConflict,
	"no_route_available":                http.StatusUnprocessableEntity,
	"metadata_too_large":                http.StatusBadRequest,
	// authentication_error
	"invalid_api_key":          http.StatusUnauthorized,
	"api_key_invalid":          http.StatusUnauthorized,
	"api_key_revoked":          http.StatusUnauthorized,
	"api_key_expired":          http.StatusUnauthorized,
	"signature_missing":        http.StatusUnauthorized,
	"signature_invalid":        http.StatusUnauthorized,
	"timestamp_out_of_window":  http.StatusUnauthorized,
	"signature_replayed":       http.StatusUnauthorized,
	"insufficient_permissions": http.StatusForbidden,
	"insufficient_scope":       http.StatusForbidden,
	"merchant_suspended":       http.StatusForbidden,
	"merchant_closed":          http.StatusForbidden,
	"livemode_mismatch":        http.StatusForbidden,
	// idempotency_error
	"idempotency_key_missing":          http.StatusBadRequest,
	"idempotency_key_invalid":          http.StatusBadRequest,
	"idempotency_key_in_use":           http.StatusConflict,
	"idempotency_key_payload_mismatch": http.StatusConflict,
	// rate_limit_error
	"rate_limit_exceeded":        http.StatusTooManyRequests,
	"concurrency_limit_exceeded": http.StatusTooManyRequests,
	// provider_error
	"card_declined":           http.StatusPaymentRequired,
	"insufficient_funds":      http.StatusPaymentRequired,
	"expired_card":            http.StatusPaymentRequired,
	"fraud_suspected":         http.StatusPaymentRequired,
	"authentication_required": http.StatusPaymentRequired,
	"authentication_failed":   http.StatusPaymentRequired,
	"provider_unavailable":    http.StatusServiceUnavailable,
	"provider_timeout":        http.StatusGatewayTimeout,
	"provider_rejected":       http.StatusBadGateway,
	// api_error
	"internal_error":          http.StatusInternalServerError,
	"concurrent_modification": http.StatusConflict,
	"service_unavailable":     http.StatusServiceUnavailable,
	"timeout":                 http.StatusGatewayTimeout,
}

// HTTPStatus 依 (type, code) 查表；未知 code 依 type 給預設。
func HTTPStatus(typ, code string) int {
	if s, ok := codeStatus[code]; ok {
		return s
	}
	switch typ {
	case TypeInvalidRequest:
		return http.StatusBadRequest
	case TypeAuthentication:
		return http.StatusUnauthorized
	case TypeIdempotency:
		return http.StatusConflict
	case TypeRateLimit:
		return http.StatusTooManyRequests
	case TypeProvider:
		return http.StatusPaymentRequired
	default:
		return http.StatusInternalServerError
	}
}

// 常用的共用錯誤（各服務可直接 errors.Is 比對或 WithParam / WithMessage 客製）。
var (
	ErrInternal            = New(TypeAPI, "internal_error", "An unexpected error occurred.")
	ErrServiceUnavailable  = New(TypeAPI, "service_unavailable", "A dependency is temporarily unavailable.")
	ErrTimeout             = New(TypeAPI, "timeout", "The request timed out.")
	ErrConcurrentModify    = New(TypeAPI, "concurrent_modification", "The resource was modified concurrently; retry the request.")
	ErrResourceMissing     = New(TypeInvalidRequest, "resource_missing", "No such resource.")
	ErrParameterMissing    = New(TypeInvalidRequest, "parameter_missing", "A required parameter is missing.")
	ErrParameterInvalid    = New(TypeInvalidRequest, "parameter_invalid", "A parameter is invalid.")
	ErrIdempotencyMissing  = New(TypeIdempotency, "idempotency_key_missing", "Idempotency-Key header is required for this request.")
	ErrIdempotencyInvalid  = New(TypeIdempotency, "idempotency_key_invalid", "Idempotency-Key must be 1-255 printable characters.")
	ErrIdempotencyInUse    = New(TypeIdempotency, "idempotency_key_in_use", "A request with the same Idempotency-Key is still in progress.")
	ErrIdempotencyMismatch = New(TypeIdempotency, "idempotency_key_payload_mismatch", "Idempotency-Key was already used with a different payload.")
	ErrRateLimited         = New(TypeRateLimit, "rate_limit_exceeded", "Too many requests.")
)

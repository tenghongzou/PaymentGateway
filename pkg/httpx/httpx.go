// Package httpx 提供 REST 錯誤格式（docs/01 §8）、JSON 輸出與共用 middleware。
package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
)

// 常用 header 名稱。
const (
	HeaderRequestID          = "X-Request-Id"
	HeaderIdempotencyKey     = "Idempotency-Key"
	HeaderIdempotentReplayed = "Idempotent-Replayed"
	HeaderRetryAfter         = "Retry-After"
)

// ErrorBody 為錯誤回應的 JSON 結構。
type ErrorBody struct {
	Error ErrorObject `json:"error"`
}

// ErrorObject 為 error 物件。
type ErrorObject struct {
	Type        string  `json:"type"`
	Code        string  `json:"code"`
	Message     string  `json:"message"`
	Param       *string `json:"param"`
	RequestID   string  `json:"request_id"`
	DeclineCode string  `json:"decline_code,omitempty"`
	Retryable   *bool   `json:"retryable,omitempty"`
}

// WriteJSON 輸出 JSON（Content-Type: application/json; charset=utf-8）。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"type":"api_error","code":"internal_error","message":"encode response"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		// client 已斷線；無法再回報，僅忽略。
		return
	}
}

// WriteError 輸出統一錯誤格式。
func WriteError(w http.ResponseWriter, status int, typ, code, message, param, requestID string) {
	var p *string
	if param != "" {
		p = &param
	}
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		if w.Header().Get(HeaderRetryAfter) == "" {
			w.Header().Set(HeaderRetryAfter, "1")
		}
	}
	WriteJSON(w, status, ErrorBody{Error: ErrorObject{Type: typ, Code: code, Message: message, Param: p, RequestID: requestID}})
}

// WriteAppError 把任意 error 轉成 REST 錯誤（apperr / gRPC status / 其他 → 500）。
func WriteAppError(w http.ResponseWriter, r *http.Request, err error) {
	e := ErrorFromGRPC(err)
	if e.HTTPStatus() >= 500 {
		logx.FromContext(r.Context()).Error("request failed", "err", err, "code", e.Code)
	}
	WriteError(w, e.HTTPStatus(), e.Type, e.Code, e.Message, e.Param, RequestIDFromRequest(r))
}

// ErrorFromGRPC 把 gRPC status（或任何 error）轉回 *apperr.Error。
func ErrorFromGRPC(err error) *apperr.Error {
	if e := grpcx.ToAppError(err); e != nil {
		return e
	}
	return apperr.ErrInternal
}

// RequestIDFromRequest 取出 request id（由 RequestID middleware 放入 context）。
func RequestIDFromRequest(r *http.Request) string {
	return logx.RequestIDFromContext(r.Context())
}

// DecodeJSON 解析 JSON body（拒絕未知欄位以外的語法錯誤，限制 1MB）。
func DecodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return apperr.ErrParameterInvalid.WithMessage("unable to read request body")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(v); err != nil {
		return apperr.ErrParameterInvalid.WithMessage("malformed JSON body: %v", err)
	}
	return nil
}

// ---- middleware ----

// RequestID 讀取或產生 X-Request-Id，放入 context 與回應 header。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get(HeaderRequestID)
		if rid == "" || len(rid) > 128 {
			rid = ids.New(ids.PrefixRequest)
		}
		w.Header().Set(HeaderRequestID, rid)
		next.ServeHTTP(w, r.WithContext(logx.WithRequestID(r.Context(), rid)))
	})
}

// Recover 把 panic 轉成 500。
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler { //nolint:errorlint // 標準庫約定以 == 比對哨兵值
						panic(rec)
					}
					logx.WithTrace(r.Context(), log).Error("http handler panic", "panic", rec, "path", r.URL.Path)
					WriteError(w, http.StatusInternalServerError, apperr.TypeAPI, "internal_error", "An unexpected error occurred.", "", RequestIDFromRequest(r))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Logging 記錄每個請求（方法、路徑、狀態、耗時），並把 logger 放進 context。
func Logging(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx := logx.IntoContext(r.Context(), log)
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r.WithContext(ctx))
			l := logx.FromContext(ctx).With(
				"http.method", r.Method, "http.path", r.URL.Path, "http.status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(), "bytes", sw.bytes, "remote", r.RemoteAddr,
			)
			switch {
			case sw.status >= 500:
				l.Error("http request")
			case sw.status >= 400:
				l.Info("http request")
			default:
				l.Debug("http request")
			}
		})
	}
}

// Timeout 為每個請求加上 deadline；逾時回 504。
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"error":{"type":"api_error","code":"timeout","message":"The request timed out."}}`)
	}
}

// statusWriter 記錄回應狀態碼與 bytes。
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

// WriteHeader 記錄狀態碼。
func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap 支援 http.ResponseController。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ReadBody 讀取 body 並放回（供簽章 / request hash 等需要多次讀取的 middleware）。
func ReadBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("httpx: request body too large")
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// ParseIntQuery 解析整數 query 參數（空值回預設）。
func ParseIntQuery(r *http.Request, name string, def int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, apperr.ErrParameterInvalid.WithMessage("%s must be an integer", name).WithParam(name)
	}
	return n, nil
}

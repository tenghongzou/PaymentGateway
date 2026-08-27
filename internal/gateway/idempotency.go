package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"unicode"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/httpx"
	"github.com/tenghongzou/paymentgateway/pkg/idempotency"
)

// errorsIs 為 errors.Is 的別名（避免與套件內變數名衝突）。
var errorsIs = errors.Is

// validIdempotencyKey 檢查 1..255 可見字元。
func validIdempotencyKey(k string) bool {
	if k == "" || len(k) > 255 {
		return false
	}
	for _, r := range k {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// requestHash = sha256(method + "\n" + path + "\n" + canonical_json(body))（docs/05 §10.3）。
func requestHash(method, path string, body []byte) string {
	canon := canonicalJSON(body)
	h := sha256.New()
	_, _ = h.Write([]byte(method))
	_, _ = h.Write([]byte{'\n'})
	_, _ = h.Write([]byte(path))
	_, _ = h.Write([]byte{'\n'})
	_, _ = h.Write(canon)
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON 排序 key、去除空白；非合法 JSON 時原樣回傳。
func canonicalJSON(body []byte) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return []byte("{}")
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return body
	}
	out, err := json.Marshal(v) // encoding/json 會排序 map key
	if err != nil {
		return body
	}
	return out
}

// recorder 擷取回應供快取。
type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
	wrote  bool
}

// WriteHeader 記錄狀態碼。
func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	_, _ = r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}

// Idempotency 為寫入端點的冪等 middleware（docs/05 §10）。
func (g *Gateway) Idempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get(httpx.HeaderIdempotencyKey)
		if key == "" {
			httpx.WriteAppError(w, r, apperr.ErrIdempotencyMissing)
			return
		}
		if !validIdempotencyKey(key) {
			httpx.WriteAppError(w, r, apperr.ErrIdempotencyInvalid)
			return
		}
		principal := PrincipalFromContext(r.Context())
		if principal == nil {
			httpx.WriteAppError(w, r, errInvalidAPIKey)
			return
		}
		body, err := httpx.ReadBody(r, 1<<20)
		if err != nil {
			httpx.WriteAppError(w, r, apperr.ErrParameterInvalid.WithMessage("request body exceeds 1 MiB"))
			return
		}
		hash := requestHash(r.Method, r.URL.Path, body)
		ctx := r.Context()
		state, cached, err := g.idem.Begin(ctx, principal.MerchantID, key, hash)
		switch {
		case errors.Is(err, idempotency.ErrMismatch):
			httpx.WriteAppError(w, r, apperr.ErrIdempotencyMismatch)
			return
		case errors.Is(err, idempotency.ErrInProgress):
			w.Header().Set(httpx.HeaderRetryAfter, "1")
			httpx.WriteAppError(w, r, apperr.ErrIdempotencyInUse)
			return
		case err != nil:
			// Valkey 不可用：fail-closed（docs/05 §10.3）。
			httpx.WriteAppError(w, r, apperr.ErrServiceUnavailable.Wrap(err))
			return
		}
		if state == idempotency.StateCompleted && cached != nil {
			for k, v := range cached.Headers {
				w.Header().Set(k, v)
			}
			w.Header().Set(httpx.HeaderIdempotentReplayed, "true")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(cached.StatusCode)
			if _, werr := w.Write(cached.Body); werr != nil { //nolint:gosec // 回放的是本服務先前產生的 JSON 回應（Content-Type 固定 application/json）
				g.log.Debug("write replayed response", "err", werr)
			}
			return
		}

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		r.Header.Set(headerRequestHash, hash)
		next.ServeHTTP(rec, r)

		if rec.status >= 500 {
			// 不快取 5xx，讓商戶能以同 key 重試。
			if aerr := g.idem.Abort(ctx, principal.MerchantID, key); aerr != nil {
				g.log.Warn("idempotency abort failed", "err", aerr)
			}
			return
		}
		headers := map[string]string{}
		if rid := w.Header().Get(httpx.HeaderRequestID); rid != "" {
			headers["X-Original-Request-Id"] = rid
		}
		if err := g.idem.Complete(ctx, principal.MerchantID, key, idempotency.Response{StatusCode: rec.status, Headers: headers, Body: rec.buf.Bytes()}); err != nil {
			g.log.Warn("idempotency complete failed", "err", err, "status", strconv.Itoa(rec.status))
		}
	})
}

// headerRequestHash 為 middleware 傳給 handler 的內部 header（不對外）。
const headerRequestHash = "X-PG-Request-Hash"

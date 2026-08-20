// Package gateway 為 api-gateway 的 HTTP 層：middleware（auth / idempotency / rate limit）、REST handlers 與 REST↔gRPC 轉換。
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	merchantv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/merchant/v1"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/httpx"
	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

// Principal 為驗證通過的呼叫者。
type Principal struct {
	MerchantID            string
	APIKeyID              string
	LiveMode              bool
	Scopes                []string
	MerchantStatus        merchantv1.MerchantStatus
	SigningSecret         string
	PreviousSigningSecret string
}

// HasScope 檢查 scope（空 scopes 表示全部允許）。
func (p *Principal) HasScope(scope string) bool {
	if len(p.Scopes) == 0 {
		return true
	}
	for _, s := range p.Scopes {
		if s == scope || s == "*" {
			return true
		}
	}
	return false
}

type principalKey struct{}

// PrincipalFromContext 取出 Principal。
func PrincipalFromContext(ctx context.Context) *Principal {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	if !ok {
		return nil
	}
	return p
}

// KeyVerifier 驗證 API key 並回傳 Principal（含 signing secret）。
type KeyVerifier interface {
	VerifyKey(ctx context.Context, apiKey string) (*Principal, error)
}

// ReplayDetector 記錄已見過的簽章（docs/06 §3.3 第 8 點）。
type ReplayDetector interface {
	// Seen 回傳 true 表示同一簽章在時間窗內已出現過；否則記錄並回 false。
	Seen(ctx context.Context, keyID, sigFragment string) (bool, error)
}

// 認證錯誤（docs/03 §7.2）。
var (
	errInvalidAPIKey     = apperr.New(apperr.TypeAuthentication, "invalid_api_key", "Invalid API key.")
	errAPIKeyRevoked     = apperr.New(apperr.TypeAuthentication, "api_key_revoked", "The API key has been revoked.")
	errAPIKeyExpired     = apperr.New(apperr.TypeAuthentication, "api_key_expired", "The API key has expired.")
	errSignatureMissing  = apperr.New(apperr.TypeAuthentication, "signature_missing", "X-Timestamp and X-Signature (v1=<hex>) headers are required.")
	errSignatureInvalid  = apperr.New(apperr.TypeAuthentication, "signature_invalid", "Request signature does not match.")
	errTimestampWindow   = apperr.New(apperr.TypeAuthentication, "timestamp_out_of_window", "X-Timestamp is outside the allowed 300 second window.")
	errSignatureReplayed = apperr.New(apperr.TypeAuthentication, "signature_replayed", "This signature was already used.")
	errInsufficientPerms = apperr.New(apperr.TypeAuthentication, "insufficient_permissions", "The API key does not have the required scope.")
	errMerchantSuspended = apperr.New(apperr.TypeAuthentication, "merchant_suspended", "The merchant account is suspended; new payments are not allowed.")
	errMerchantClosed    = apperr.New(apperr.TypeAuthentication, "merchant_closed", "The merchant account is closed; write operations are not allowed.")
)

// DevVerifier 為開發模式驗證器（PG_ENV=dev 且設定 PG_DEV_*）：不呼叫 merchant-service。
type DevVerifier struct {
	APIKey        string
	SigningSecret string
	MerchantID    string
}

// VerifyKey 以常數時間比對開發用 API key。
func (d *DevVerifier) VerifyKey(_ context.Context, apiKey string) (*Principal, error) {
	if subtle.ConstantTimeCompare([]byte(apiKey), []byte(d.APIKey)) != 1 {
		return nil, errInvalidAPIKey
	}
	return &Principal{
		MerchantID: d.MerchantID, APIKeyID: "key_dev", LiveMode: strings.HasPrefix(apiKey, "pk_live_"),
		MerchantStatus: merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE, SigningSecret: d.SigningSecret,
	}, nil
}

// GRPCVerifier 透過 merchant-service VerifyApiKey 驗證，結果快取 60s。
type GRPCVerifier struct {
	Client merchantv1.MerchantServiceClient
	TTL    time.Duration
	mu     sync.Mutex
	cache  map[string]cachedPrincipal
	now    func() time.Time
}

type cachedPrincipal struct {
	p         *Principal
	err       error
	expiresAt time.Time
}

// NewGRPCVerifier 建立 GRPCVerifier。
func NewGRPCVerifier(client merchantv1.MerchantServiceClient) *GRPCVerifier {
	return &GRPCVerifier{Client: client, TTL: 60 * time.Second, cache: map[string]cachedPrincipal{}, now: time.Now}
}

// VerifyKey 實作 KeyVerifier。
func (g *GRPCVerifier) VerifyKey(ctx context.Context, apiKey string) (*Principal, error) {
	sum := sha256.Sum256([]byte(apiKey))
	ck := hex.EncodeToString(sum[:])
	g.mu.Lock()
	if c, ok := g.cache[ck]; ok && g.now().Before(c.expiresAt) {
		g.mu.Unlock()
		return c.p, c.err
	}
	g.mu.Unlock()

	prefix := apiKey
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	resp, err := g.Client.VerifyApiKey(ctx, &merchantv1.VerifyApiKeyRequest{KeyPrefix: prefix, Key: apiKey})
	if err != nil {
		return nil, httpx.ErrorFromGRPC(err)
	}
	var p *Principal
	var verr error
	if !resp.GetValid() {
		switch strings.ToLower(resp.GetReason()) {
		case "revoked", "api_key_revoked":
			verr = errAPIKeyRevoked
		case "expired", "api_key_expired":
			verr = errAPIKeyExpired
		default:
			verr = errInvalidAPIKey
		}
	} else {
		p = &Principal{
			MerchantID: resp.GetMerchantId(), APIKeyID: resp.GetApiKeyId(), LiveMode: resp.GetMode() == merchantv1.ApiKeyMode_API_KEY_MODE_LIVE,
			Scopes: resp.GetScopes(), MerchantStatus: resp.GetMerchantStatus(),
			SigningSecret: resp.GetSigningSecret(), PreviousSigningSecret: resp.GetPreviousSigningSecret(),
		}
	}
	g.mu.Lock()
	g.cache[ck] = cachedPrincipal{p: p, err: verr, expiresAt: g.now().Add(g.TTL)}
	g.mu.Unlock()
	return p, verr
}

// MemoryReplayDetector 為記憶體實作（測試 / 單機）。
type MemoryReplayDetector struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	now  func() time.Time
}

// NewMemoryReplayDetector 建立記憶體實作。
func NewMemoryReplayDetector() *MemoryReplayDetector {
	return &MemoryReplayDetector{seen: map[string]time.Time{}, ttl: sig.DefaultWindow, now: time.Now}
}

// Seen 實作 ReplayDetector。
func (m *MemoryReplayDetector) Seen(_ context.Context, keyID, frag string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	k := keyID + ":" + frag
	if exp, ok := m.seen[k]; ok && now.Before(exp) {
		return true, nil
	}
	m.seen[k] = now.Add(m.ttl)
	if len(m.seen) > 100000 {
		for kk, exp := range m.seen {
			if now.After(exp) {
				delete(m.seen, kk)
			}
		}
	}
	return false, nil
}

// scopeFor 依路由決定所需 scope。
func scopeFor(r *http.Request) string {
	write := r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/refunds"):
		if write {
			return "refunds:write"
		}
		return "refunds:read"
	default:
		if write {
			return "payments:write"
		}
		return "payments:read"
	}
}

// Auth 為認證 middleware：Bearer key → 簽章 → 重放 → 商戶狀態 → scope。
func (g *Gateway) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		authz := r.Header.Get("Authorization")
		scheme, key, ok := strings.Cut(authz, " ")
		key = strings.TrimSpace(key)
		if !ok || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(key, "pk_") {
			httpx.WriteAppError(w, r, errInvalidAPIKey)
			return
		}
		principal, err := g.verifier.VerifyKey(ctx, key)
		if err != nil {
			httpx.WriteAppError(w, r, err)
			return
		}

		ts := r.Header.Get("X-Timestamp")
		signature := r.Header.Get("X-Signature")
		body, err := httpx.ReadBody(r, 1<<20)
		if err != nil {
			httpx.WriteAppError(w, r, apperr.ErrParameterInvalid.WithMessage("request body exceeds 1 MiB"))
			return
		}
		secrets := []string{principal.SigningSecret}
		if principal.PreviousSigningSecret != "" {
			secrets = append(secrets, principal.PreviousSigningSecret)
		}
		if err := sig.VerifyAny(secrets, ts, signature, r.Method, r.URL.RequestURI(), body, g.clock(), sig.DefaultWindow); err != nil {
			httpx.WriteAppError(w, r, mapSigError(err))
			return
		}
		seen, rerr := g.replay.Seen(ctx, principal.APIKeyID, sig.ReplayKey(signature))
		if rerr != nil {
			httpx.WriteAppError(w, r, apperr.ErrServiceUnavailable.Wrap(rerr))
			return
		}
		if seen {
			httpx.WriteAppError(w, r, errSignatureReplayed)
			return
		}

		// 商戶狀態（docs/03 §2.5）：suspended 不可建立付款；closed 拒絕所有寫入。
		write := r.Method != http.MethodGet && r.Method != http.MethodHead
		switch principal.MerchantStatus {
		case merchantv1.MerchantStatus_MERCHANT_STATUS_CLOSED:
			if write {
				httpx.WriteAppError(w, r, errMerchantClosed)
				return
			}
		case merchantv1.MerchantStatus_MERCHANT_STATUS_SUSPENDED:
			if r.Method == http.MethodPost && r.URL.Path == "/v1/payments" {
				httpx.WriteAppError(w, r, errMerchantSuspended)
				return
			}
		case merchantv1.MerchantStatus_MERCHANT_STATUS_ACTIVE, merchantv1.MerchantStatus_MERCHANT_STATUS_UNSPECIFIED:
		}
		if !principal.HasScope(scopeFor(r)) {
			httpx.WriteAppError(w, r, errInsufficientPerms)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, principalKey{}, principal)))
	})
}

func mapSigError(err error) error {
	switch {
	case err == nil:
		return nil
	case errorsIs(err, sig.ErrSignatureMissing), errorsIs(err, sig.ErrTimestampInvalid):
		return errSignatureMissing
	case errorsIs(err, sig.ErrTimestampOutOfWindow):
		return errTimestampWindow
	default:
		return errSignatureInvalid
	}
}

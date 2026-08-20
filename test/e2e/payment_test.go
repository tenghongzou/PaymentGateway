//go:build e2e

// Package e2e 對本機 api-gateway（預設 http://localhost:8080）跑端到端測試；需先 `make compose-up` 或以 go run 啟動三個服務。
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	gatewayURL = envOr("PG_E2E_GATEWAY_URL", "http://localhost:8080")
	apiKey     = envOr("PG_DEV_API_KEY", "pk_test_dev_0000000000000000")
	secret     = envOr("PG_DEV_SIGNING_SECRET", "sk_test_dev_secret_change_me")
)

// usedSignatures 避免同一秒內對相同 (method, target, body) 重複簽出同一簽章而被 signature_replayed 拒絕
// （簽章 canonical 不含 nonce；真實商戶 SDK 遇到相同請求也必須等下一秒或改變內容）。
var (
	usedMu         sync.Mutex
	usedSignatures = map[string]bool{}
)

func freshSignature(method, target string, raw []byte) (ts, signature string) {
	usedMu.Lock()
	defer usedMu.Unlock()
	for {
		ts = strconv.FormatInt(time.Now().Unix(), 10)
		signature = sig.Sign(secret, ts, method, target, raw)
		if !usedSignatures[signature] {
			usedSignatures[signature] = true
			return ts, signature
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func call(t *testing.T, method, target string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	ts, signature := freshSignature(method, target, raw)
	req, err := http.NewRequest(method, gatewayURL+target, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", signature)
	req.Header.Set("Content-Type", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var out map[string]any
	if len(b) > 0 {
		require.NoError(t, json.Unmarshal(b, &out), string(b))
	}
	return resp.StatusCode, out
}

func createBody(token string) map[string]any {
	return map[string]any{
		"amount":         map[string]any{"amount_minor": 1000, "currency": "TWD"},
		"capture_method": "automatic",
		"payment_method": map[string]any{"type": "card", "card": map[string]any{"token": token, "token_provider": "mock"}},
		"customer":       map[string]any{"id": "cus_e2e", "email": "e2e@example.com"},
		"metadata":       map[string]any{"suite": "e2e"},
	}
}

func TestHealthz(t *testing.T) {
	resp, err := http.Get(gatewayURL + "/healthz")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
}

func TestCreateGetRefund(t *testing.T) {
	code, p := call(t, http.MethodPost, "/v1/payments", createBody("tok_ok"))
	require.Equal(t, 201, code, fmt.Sprint(p))
	assert.Equal(t, "captured", p["status"])
	id := asStr(t, p["id"])

	code, got := call(t, http.MethodGet, "/v1/payments/"+id, nil)
	require.Equal(t, 200, code)
	assert.Equal(t, "captured", got["status"])
	assert.Equal(t, "mock", got["provider"])
	assert.Len(t, got["attempts"], 1)

	code, r := call(t, http.MethodPost, "/v1/refunds", map[string]any{"payment_id": id, "amount": map[string]any{"amount_minor": 400, "currency": "TWD"}, "reason": "requested_by_customer"})
	require.Equal(t, 201, code, fmt.Sprint(r))
	assert.Equal(t, "succeeded", r["status"])
	rid := asStr(t, r["id"])

	code, got = call(t, http.MethodGet, "/v1/refunds/"+rid, nil)
	require.Equal(t, 200, code)
	assert.Equal(t, "succeeded", got["status"])

	code, got = call(t, http.MethodGet, "/v1/payments/"+id, nil)
	require.Equal(t, 200, code)
	assert.Equal(t, "partially_refunded", got["status"])
	assert.InDelta(t, 400, asMap(t, got["refunded_amount"])["amount_minor"], 0)
}

func TestHardDecline(t *testing.T) {
	code, p := call(t, http.MethodPost, "/v1/payments", createBody("tok_decline_hard"))
	require.Equal(t, 201, code, fmt.Sprint(p))
	assert.Equal(t, "failed", p["status"])
	le := asMap(t, p["last_error"])
	assert.Equal(t, "provider_error", le["type"])
	assert.Len(t, p["attempts"], 1, "hard decline must not failover")
}

func TestUnavailableOnceRecovers(t *testing.T) {
	code, p := call(t, http.MethodPost, "/v1/payments", createBody("tok_unavailable_once"))
	require.Equal(t, 201, code, fmt.Sprint(p))
	assert.Equal(t, "captured", p["status"])
	assert.Len(t, p["attempts"], 2, "first attempt unavailable, second approved")
}

func TestManualCaptureAndVoid(t *testing.T) {
	body := createBody("tok_ok")
	body["capture_method"] = "manual"
	code, p := call(t, http.MethodPost, "/v1/payments", body)
	require.Equal(t, 201, code, fmt.Sprint(p))
	assert.Equal(t, "authorized", p["status"])
	id := asStr(t, p["id"])
	code, v := call(t, http.MethodPost, "/v1/payments/"+id+"/void", map[string]any{"reason": "requested_by_customer"})
	require.Equal(t, 200, code, fmt.Sprint(v))
	assert.Equal(t, "voided", v["status"])

	code, p2 := call(t, http.MethodPost, "/v1/payments", body)
	require.Equal(t, 201, code)
	id2 := asStr(t, p2["id"])
	code, c := call(t, http.MethodPost, "/v1/payments/"+id2+"/capture", map[string]any{"amount": map[string]any{"amount_minor": 600, "currency": "TWD"}})
	require.Equal(t, 200, code, fmt.Sprint(c))
	assert.Equal(t, "captured", c["status"])
	assert.InDelta(t, 600, asMap(t, c["captured_amount"])["amount_minor"], 0)
}

func Test3DSConfirm(t *testing.T) {
	code, p := call(t, http.MethodPost, "/v1/payments", createBody("tok_3ds"))
	require.Equal(t, 201, code, fmt.Sprint(p))
	assert.Equal(t, "requires_action", p["status"])
	na := asMap(t, p["next_action"])
	assert.Equal(t, "redirect", na["type"])
	id := asStr(t, p["id"])
	code, c := call(t, http.MethodPost, "/v1/payments/"+id+"/confirm", map[string]any{"provider_params": map[string]any{"result": "ok"}})
	require.Equal(t, 200, code, fmt.Sprint(c))
	assert.Equal(t, "captured", c["status"])
}

func TestAuthRejected(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, gatewayURL+"/v1/payments/pay_x", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-Signature", "v1="+string(bytes.Repeat([]byte("0"), 64)))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 401, resp.StatusCode)
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	require.True(t, ok, "expected object, got %T", v)
	return m
}

func asStr(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	require.True(t, ok, "expected string, got %T", v)
	return s
}

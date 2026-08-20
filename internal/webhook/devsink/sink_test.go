package devsink

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

func TestHandler(t *testing.T) {
	var out bytes.Buffer
	h := Handler(Options{Secrets: []string{"whsec_test"}, FailFirst: 1, FailStatus: 503, Out: &out})
	body := []byte(`{"id":"evt_1","type":"payment.captured","data":{"object":{"id":"pay_1"}}}`)

	post := func(signature string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-PG-Signature", signature)
		req.Header.Set("X-PG-Event-Id", "evt_1")
		req.Header.Set("X-PG-Event-Type", "payment.captured")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	good := sig.SignWebhook("whsec_test", time.Now().Unix(), body)
	assert.Equal(t, 503, post(good).Code, "第一次模擬失敗")
	assert.Equal(t, 200, post(good).Code)
	assert.Equal(t, 400, post(sig.SignWebhook("wrong", time.Now().Unix(), body)).Code)
	assert.Equal(t, 400, post("").Code)
	assert.Equal(t, 400, post(sig.SignWebhook("whsec_test", time.Now().Add(-time.Hour).Unix(), body)).Code, "過期時間戳")

	s := out.String()
	assert.Contains(t, s, "signature: ok")
	assert.Contains(t, s, "signature: FAILED")
	assert.Contains(t, s, `"type": "payment.captured"`)
	assert.Equal(t, 5, strings.Count(s, "=== webhook #"))

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

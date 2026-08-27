package http

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

func signedRequest(t *testing.T, url string, secrets []string, body []byte) app.SendRequest {
	t.Helper()
	h, err := domain.Signer{}.Sign(secrets, time.Now().Unix(), body)
	require.NoError(t, err)
	return app.SendRequest{URL: url, Body: body, Headers: map[string]string{
		"Content-Type": "application/json", "User-Agent": domain.UserAgent,
		domain.HeaderSignature: h, domain.HeaderEventID: "evt_1", domain.HeaderEventType: "payment.captured",
		domain.HeaderDeliveryID: "whd_1", domain.HeaderAttempt: "1",
	}}
}

func TestSender_SignatureVerifiableByMerchant(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"payment.captured","data":{"object":{"id":"pay_1"}}}`)
	var gotHeaders http.Header
	var verifyErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		raw, rerr := io.ReadAll(r.Body)
		assert.NoError(t, rerr)
		verifyErr = sig.VerifyWebhook([]string{"whsec_previous"}, r.Header.Get("X-PG-Signature"), raw, time.Now(), 0)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, body, raw)
		w.WriteHeader(http.StatusOK)
		_, werr := w.Write([]byte(`{"received":true}`))
		assert.NoError(t, werr)
	}))
	defer srv.Close()

	s := NewSender(Options{Policy: domain.DevPolicy})
	out := s.Send(context.Background(), signedRequest(t, srv.URL+"/hooks", []string{"whsec_current", "whsec_previous"}, body))
	require.NoError(t, out.Err)
	assert.Equal(t, 200, out.StatusCode)
	assert.True(t, out.Succeeded())
	assert.Equal(t, `{"received":true}`, out.Body)
	assert.Greater(t, out.Duration, time.Duration(0))
	require.NoError(t, verifyErr, "商戶用 previous secret 也能驗過")
	assert.Equal(t, "application/json", gotHeaders.Get("Content-Type"))
	assert.Equal(t, "PaymentGateway-Webhooks/1.0", gotHeaders.Get("User-Agent"))
	assert.Equal(t, "evt_1", gotHeaders.Get("X-PG-Event-Id"))
	assert.Equal(t, "payment.captured", gotHeaders.Get("X-PG-Event-Type"))
	assert.Equal(t, "whd_1", gotHeaders.Get("X-PG-Delivery-Id"))
	assert.Equal(t, "1", gotHeaders.Get("X-PG-Attempt"))
	assert.Equal(t, 2, strings.Count(gotHeaders.Get("X-PG-Signature"), "v1="))
}

func TestSender_TLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	tr, ok := srv.Client().Transport.(*http.Transport)
	require.True(t, ok)
	tlsCfg := tr.TLSClientConfig.Clone()
	s := NewSender(Options{Policy: domain.DevPolicy, TLSConfig: tlsCfg})
	out := s.Send(context.Background(), signedRequest(t, srv.URL, []string{"s"}, []byte(`{}`)))
	require.NoError(t, out.Err)
	assert.Equal(t, 204, out.StatusCode)
	assert.True(t, out.Succeeded())
	// 未信任的憑證 → 失敗（不是 2xx）。
	s2 := NewSender(Options{Policy: domain.DevPolicy, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}})
	out = s2.Send(context.Background(), signedRequest(t, srv.URL, []string{"s"}, []byte(`{}`)))
	require.Error(t, out.Err)
	assert.Equal(t, 0, out.StatusCode)
}

func TestSender_NoRedirectFollow(t *testing.T) {
	var followed atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/other", http.StatusFound) })
	mux.HandleFunc("/other", func(w http.ResponseWriter, _ *http.Request) { followed.Add(1); w.WriteHeader(200) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	out := NewSender(Options{Policy: domain.DevPolicy}).Send(context.Background(), signedRequest(t, srv.URL+"/hook", []string{"s"}, []byte(`{}`)))
	require.NoError(t, out.Err)
	assert.Equal(t, 302, out.StatusCode)
	assert.False(t, out.Succeeded())
	assert.EqualValues(t, 0, followed.Load())
}

func TestSender_RetryAfterAndBodyTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/429":
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
		case "/429date":
			w.Header().Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(429)
		case "/big":
			w.WriteHeader(500)
			_, werr := w.Write([]byte(strings.Repeat("x", 10_000)))
			assert.NoError(t, werr)
		}
	}))
	defer srv.Close()
	s := NewSender(Options{Policy: domain.DevPolicy})
	out := s.Send(context.Background(), signedRequest(t, srv.URL+"/429", []string{"s"}, []byte(`{}`)))
	assert.Equal(t, 429, out.StatusCode)
	assert.Equal(t, 2*time.Minute, out.RetryAfter)
	out = s.Send(context.Background(), signedRequest(t, srv.URL+"/429date", []string{"s"}, []byte(`{}`)))
	assert.InDelta(t, 90*time.Second, out.RetryAfter, float64(5*time.Second))
	out = s.Send(context.Background(), signedRequest(t, srv.URL+"/big", []string{"s"}, []byte(`{}`)))
	assert.Equal(t, 500, out.StatusCode)
	assert.Len(t, out.Body, domain.MaxResponseBodyBytes)
}

func TestSender_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	s := NewSender(Options{Policy: domain.DevPolicy, Timeout: 150 * time.Millisecond})
	out := s.Send(context.Background(), signedRequest(t, srv.URL, []string{"s"}, []byte(`{}`)))
	require.Error(t, out.Err)
	assert.Equal(t, 0, out.StatusCode)
	assert.Contains(t, out.Err.Error(), "timed out")
}

type staticResolver struct{ addr string }

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr(r.addr)}, nil
}

func TestSender_SSRFBlockedAtDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	// 嚴格政策：URL 看起來合法（https、公開網域、8443）但 DNS 解析到 loopback → 連線層拒絕。
	s := NewSender(Options{Policy: domain.StrictPolicy, Resolver: staticResolver{"127.0.0.1"}})
	out := s.Send(context.Background(), signedRequest(t, "https://hooks.example.com:8443/x", []string{"s"}, []byte(`{}`)))
	require.Error(t, out.Err)
	require.ErrorIs(t, out.Err, domain.ErrIPNotAllowed)
	assert.Equal(t, 0, out.StatusCode)

	// 嚴格政策：IP literal / http 在 ValidateURL 就被擋。
	out = s.Send(context.Background(), signedRequest(t, "http://127.0.0.1:"+port+"/x", []string{"s"}, []byte(`{}`)))
	require.ErrorIs(t, out.Err, domain.ErrURLNotAllowed)

	// dev 政策 + 解析到 loopback 的網域名稱 → 允許並成功連線到 httptest。
	dev := NewSender(Options{Policy: domain.DevPolicy, Resolver: staticResolver{"127.0.0.1"}})
	out = dev.Send(context.Background(), signedRequest(t, "http://hooks.example.com:"+port+"/x", []string{"s"}, []byte(`{}`)))
	require.NoError(t, out.Err)
	assert.Equal(t, 200, out.StatusCode)
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Now()
	assert.Equal(t, time.Duration(0), parseRetryAfter("", now))
	assert.Equal(t, time.Duration(0), parseRetryAfter("abc", now))
	assert.Equal(t, time.Duration(0), parseRetryAfter("-5", now))
	assert.Equal(t, 7*time.Second, parseRetryAfter("7", now))
	assert.Equal(t, time.Duration(0), parseRetryAfter(now.Add(-time.Minute).UTC().Format(http.TimeFormat), now))
}

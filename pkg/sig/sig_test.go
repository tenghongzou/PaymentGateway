package sig

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalString(t *testing.T) {
	body := []byte(`{"a":1}`)
	sum := sha256.Sum256(body)
	got := CanonicalString("1700000000", "post", "/v1/payments?x=1", body)
	want := "1700000000\nPOST\n/v1/payments?x=1\n" + hex.EncodeToString(sum[:])
	assert.Equal(t, want, got)
}

func TestSignVerify(t *testing.T) {
	secret := "sk_test_secret"
	now := time.Unix(1700000000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"amount":{"amount_minor":1000,"currency":"TWD"}}`)
	s := Sign(secret, ts, "POST", "/v1/payments", body)
	assert.Len(t, s, 3+64)
	assert.True(t, strings.HasPrefix(s, "v1="))
	assert.Equal(t, s[3:], SignHex(secret, ts, "POST", "/v1/payments", body))
	assert.Equal(t, s[3:35], ReplayKey(s))
	assert.Empty(t, ReplayKey("garbage"))

	tests := []struct {
		name    string
		secret  string
		ts      string
		sig     string
		method  string
		target  string
		body    []byte
		now     time.Time
		wantErr error
	}{
		{"ok", secret, ts, s, "POST", "/v1/payments", body, now, nil},
		{"ok uppercase hex", secret, ts, "v1=" + strings.ToUpper(s[3:]), "POST", "/v1/payments", body, now, nil},
		{"missing v1 prefix", secret, ts, s[3:], "POST", "/v1/payments", body, now, ErrSignatureMissing},
		{"wrong version", secret, ts, "v2=" + s[3:], "POST", "/v1/payments", body, now, ErrSignatureMissing},
		{"not hex", secret, ts, "v1=" + strings.Repeat("z", 64), "POST", "/v1/payments", body, now, ErrSignatureMissing},
		{"ok lowercase method", secret, ts, s, "post", "/v1/payments", body, now, nil},
		{"ok within window", secret, ts, s, "POST", "/v1/payments", body, now.Add(299 * time.Second), nil},
		{"wrong secret", "other", ts, s, "POST", "/v1/payments", body, now, ErrSignatureInvalid},
		{"wrong body", secret, ts, s, "POST", "/v1/payments", []byte(`{}`), now, ErrSignatureInvalid},
		{"wrong method", secret, ts, s, "GET", "/v1/payments", body, now, ErrSignatureInvalid},
		{"wrong target", secret, ts, s, "POST", "/v1/refunds", body, now, ErrSignatureInvalid},
		{"missing sig", secret, ts, "", "POST", "/v1/payments", body, now, ErrSignatureMissing},
		{"missing ts", secret, "", s, "POST", "/v1/payments", body, now, ErrSignatureMissing},
		{"bad ts", secret, "abc", s, "POST", "/v1/payments", body, now, ErrTimestampInvalid},
		{"too old", secret, ts, s, "POST", "/v1/payments", body, now.Add(301 * time.Second), ErrTimestampOutOfWindow},
		{"too new", secret, ts, s, "POST", "/v1/payments", body, now.Add(-301 * time.Second), ErrTimestampOutOfWindow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.secret, tt.ts, tt.sig, tt.method, tt.target, tt.body, tt.now, DefaultWindow)
			if tt.wantErr == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tt.wantErr)
			}
		})
	}

	// 空 body（GET）也可簽；空 body 的 sha256 為固定值。
	gs := Sign(secret, ts, "GET", "/v1/payments/pay_1", nil)
	require.NoError(t, Verify(secret, ts, gs, "GET", "/v1/payments/pay_1", []byte{}, now, 0))
	assert.True(t, strings.HasSuffix(CanonicalString(ts, "GET", "/v1/payments", nil), "\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"))

	// 多把 secret。
	require.NoError(t, VerifyAny([]string{"", "old", secret}, ts, s, "POST", "/v1/payments", body, now, DefaultWindow))
	require.ErrorIs(t, VerifyAny([]string{"old"}, ts, s, "POST", "/v1/payments", body, now, DefaultWindow), ErrSignatureInvalid)
	require.ErrorIs(t, VerifyAny([]string{secret}, ts, s, "POST", "/v1/payments", body, now.Add(time.Hour), DefaultWindow), ErrTimestampOutOfWindow)
}

func TestWebhookSignVerify(t *testing.T) {
	now := time.Unix(1700000000, 0)
	body := []byte(`{"id":"evt_1","type":"payment.captured"}`)
	h := SignWebhook("whsec_a", now.Unix(), body)
	assert.True(t, strings.HasPrefix(h, "t=1700000000,v1="))

	require.NoError(t, VerifyWebhook([]string{"whsec_a"}, h, body, now, DefaultWindow))
	// 輪替：新 secret 排前面、舊的仍接受。
	require.NoError(t, VerifyWebhook([]string{"whsec_b", "whsec_a"}, h, body, now, DefaultWindow))
	require.ErrorIs(t, VerifyWebhook([]string{"whsec_b"}, h, body, now, DefaultWindow), ErrSignatureInvalid)
	require.ErrorIs(t, VerifyWebhook([]string{"whsec_a"}, h, []byte("tampered"), now, DefaultWindow), ErrSignatureInvalid)
	require.ErrorIs(t, VerifyWebhook([]string{"whsec_a"}, h, body, now.Add(10*time.Minute), DefaultWindow), ErrTimestampOutOfWindow)
	require.ErrorIs(t, VerifyWebhook([]string{"whsec_a"}, "garbage", body, now, DefaultWindow), ErrSignatureMissing)
	require.ErrorIs(t, VerifyWebhook([]string{"whsec_a"}, "t=abc,v1=00", body, now, DefaultWindow), ErrTimestampInvalid)

	// 多個 v1（發送端同時以兩把 secret 簽）。
	h2 := h + ",v1=" + strings.Repeat("0", 64)
	require.NoError(t, VerifyWebhook([]string{"whsec_a"}, h2, body, now, DefaultWindow))

	ts, sigs, err := ParseWebhookHeader(h2)
	require.NoError(t, err)
	assert.Equal(t, "1700000000", ts)
	assert.Len(t, sigs, 2)
}

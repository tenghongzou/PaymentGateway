package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

func TestSignerHeaderFormat(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"payment.captured"}`)
	ts := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC).Unix()

	h, err := Signer{}.Sign([]string{"whsec_current"}, ts, body)
	require.NoError(t, err)
	assert.Regexp(t, `^t=\d+,v1=[0-9a-f]{64}$`, h)
	assert.Equal(t, sig.SignWebhook("whsec_current", ts, body), h)

	// 輪替期間兩把 secret → 兩個 v1=。
	h2, err := Signer{}.Sign([]string{"whsec_current", "whsec_previous"}, ts, body)
	require.NoError(t, err)
	assert.Regexp(t, `^t=\d+,v1=[0-9a-f]{64},v1=[0-9a-f]{64}$`, h2)
	assert.Equal(t, 2, strings.Count(h2, "v1="))
	now := time.Unix(ts, 0)
	// 只持有其中一把的商戶都驗得過。
	assert.NoError(t, sig.VerifyWebhook([]string{"whsec_current"}, h2, body, now, 0))
	assert.NoError(t, sig.VerifyWebhook([]string{"whsec_previous"}, h2, body, now, 0))
	require.ErrorIs(t, sig.VerifyWebhook([]string{"whsec_other"}, h2, body, now, 0), sig.ErrSignatureInvalid)
	// 改動 body 後驗證失敗。
	require.Error(t, sig.VerifyWebhook([]string{"whsec_current"}, h2, append(body, ' '), now, 0))

	// 空 secret 被略過；全空 → ErrNoSecrets。
	h3, err := Signer{}.Sign([]string{"", "whsec_current"}, ts, body)
	require.NoError(t, err)
	assert.Equal(t, h, h3)
	_, err = Signer{}.Sign([]string{"", ""}, ts, body)
	assert.ErrorIs(t, err, ErrNoSecrets)
}

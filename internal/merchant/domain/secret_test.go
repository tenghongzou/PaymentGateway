package domain

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateSecrets(t *testing.T) {
	assert.Regexp(t, `^sk_live_[0-9A-Za-z]{43}$`, GenerateSigningSecret(ModeLive))
	assert.Regexp(t, `^sk_test_[0-9A-Za-z]{43}$`, GenerateSigningSecret(ModeTest))
	assert.Regexp(t, `^whsec_[0-9A-Za-z]{43}$`, GenerateWebhookSecret())
	assert.True(t, IsSigningSecret(GenerateSigningSecret(ModeLive)))
	assert.True(t, IsWebhookSecret(GenerateWebhookSecret()))
	assert.False(t, IsSigningSecret("sk_live_short"))
}

func TestBase62(t *testing.T) {
	assert.Equal(t, "0000000000", encodeBase62([]byte{0}, 10))
	assert.Equal(t, "00000000zz", encodeBase62([]byte{0x0f, 0x03}, 10), "61*62+61 = 3843 = 0x0f03")
	all := make([]byte, 32)
	for i := range all {
		all[i] = 0xff
	}
	assert.Len(t, encodeBase62(all, 43), 43, "2^256-1 仍在 43 碼內")
	assert.True(t, isBase62(randomBody()))
}

func TestAESGCMCipherRoundTrip(t *testing.T) {
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	c, err := NewAESGCMCipher(kek)
	require.NoError(t, err)

	aad := SecretAAD("api_keys", "signing_secret_enc", "key_1")
	ct, err := c.Encrypt("sk_live_secret", aad)
	require.NoError(t, err)
	assert.Contains(t, ct, "aesgcm:v1:")
	assert.NotContains(t, ct, "sk_live_secret")

	pt, err := c.Decrypt(ct, aad)
	require.NoError(t, err)
	assert.Equal(t, "sk_live_secret", pt)

	// AAD 不同 → 拒絕（密文不可搬到別筆記錄）
	_, err = c.Decrypt(ct, SecretAAD("api_keys", "signing_secret_enc", "key_2"))
	require.ErrorIs(t, err, ErrCiphertextInvalid)

	// 每次 nonce 不同
	ct2, err := c.Encrypt("sk_live_secret", aad)
	require.NoError(t, err)
	assert.NotEqual(t, ct, ct2)

	// 相容 plain:v1:
	pt, err = c.Decrypt("plain:v1:whsec_x", aad)
	require.NoError(t, err)
	assert.Equal(t, "whsec_x", pt)

	_, err = c.Decrypt("garbage", aad)
	require.ErrorIs(t, err, ErrCiphertextInvalid)
	_, err = c.Decrypt("aesgcm:v1:!!!", aad)
	require.ErrorIs(t, err, ErrCiphertextInvalid)

	_, err = NewAESGCMCipher([]byte("short"))
	require.Error(t, err)
	_, err = NewAESGCMCipherFromBase64(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	_, err = NewAESGCMCipherFromBase64("%%%")
	require.Error(t, err)
}

func TestPlaintextCipher(t *testing.T) {
	var c PlaintextCipher
	ct, err := c.Encrypt("whsec_abc", "aad")
	require.NoError(t, err)
	assert.Equal(t, "plain:v1:whsec_abc", ct)
	pt, err := c.Decrypt(ct, "aad")
	require.NoError(t, err)
	assert.Equal(t, "whsec_abc", pt)
	_, err = c.Decrypt("aesgcm:v1:xxx", "aad")
	require.ErrorIs(t, err, ErrKEKRequired)
	_, err = c.Decrypt("xxx", "aad")
	require.ErrorIs(t, err, ErrCiphertextInvalid)
}

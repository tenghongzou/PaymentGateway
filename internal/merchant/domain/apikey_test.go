package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateKeyFormat(t *testing.T) {
	for _, mode := range []Mode{ModeLive, ModeTest} {
		t.Run(string(mode), func(t *testing.T) {
			plain, key, err := GenerateKey(mode)
			require.NoError(t, err)
			assert.Len(t, plain, APIKeyLen, "pk_<mode>_ + 43 base62")
			assert.Regexp(t, `^pk_(live|test)_[0-9A-Za-z]{43}$`, plain)
			assert.True(t, strings.HasPrefix(plain, "pk_"+string(mode)+"_"))
			assert.Equal(t, plain[:APIKeyPrefixLen], key.Prefix)
			assert.Len(t, key.Prefix, 16)
			assert.Len(t, key.LookupID(), LookupIDLen)
			assert.Equal(t, mode, key.Mode)
			assert.NotEmpty(t, key.ID)
			assert.True(t, strings.HasPrefix(key.PublicID(), "key_"))
			// hash 不得包含明文
			assert.NotContains(t, key.KeyHash, plain[8:])
		})
	}
	_, _, err := GenerateKey("prod")
	require.Error(t, err)
}

func TestGenerateKeyUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for range 50 {
		plain, _, err := GenerateKey(ModeTest)
		require.NoError(t, err)
		_, dup := seen[plain]
		assert.False(t, dup)
		seen[plain] = struct{}{}
	}
}

func TestArgon2idPHCParams(t *testing.T) {
	plain, key, err := GenerateKey(ModeLive)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(key.KeyHash, "$argon2id$v=19$m=65536,t=3,p=4$"), key.KeyHash)
	parts := strings.Split(key.KeyHash, "$")
	require.Len(t, parts, 6)
	p, err := parseArgon2id(key.KeyHash)
	require.NoError(t, err)
	assert.Equal(t, Argon2Memory, p.memory)
	assert.Equal(t, Argon2Time, p.time)
	assert.Equal(t, Argon2Threads, p.threads)
	assert.Len(t, p.salt, Argon2SaltLen)
	assert.Len(t, p.hash, int(Argon2KeyLen))
	assert.True(t, key.Verify(plain))
}

func TestVerify(t *testing.T) {
	plain, key, err := GenerateKey(ModeLive)
	require.NoError(t, err)

	assert.True(t, key.Verify(plain))
	assert.False(t, key.Verify(plain[:len(plain)-1]+"x"), "最後一碼不同")
	assert.False(t, key.Verify(strings.Replace(plain, "pk_live_", "pk_test_", 1)), "hash 輸入含前綴")
	assert.False(t, key.Verify(""))

	// 每次 hash 用不同 salt，但都可驗證
	h2 := HashArgon2id(plain)
	assert.NotEqual(t, key.KeyHash, h2)
	assert.True(t, VerifyArgon2id(h2, plain))

	// 損毀 / 非 PHC 格式
	assert.False(t, VerifyArgon2id("not-a-hash", plain))
	assert.False(t, VerifyArgon2id("$argon2i$v=19$m=65536,t=3,p=4$AAAA$BBBB", plain))
	assert.False(t, VerifyArgon2id("$argon2id$v=18$m=65536,t=3,p=4$AAAA$BBBB", plain))
}

func TestParseKey(t *testing.T) {
	plain, key, err := GenerateKey(ModeTest)
	require.NoError(t, err)
	mode, prefix, err := ParseKey(plain)
	require.NoError(t, err)
	assert.Equal(t, ModeTest, mode)
	assert.Equal(t, key.Prefix, prefix)

	for _, bad := range []string{"", "pk_test_short", "sk_test_" + strings.Repeat("a", 43), "pk_prod_" + strings.Repeat("a", 43), "pk_test_" + strings.Repeat("-", 43)} {
		_, _, err := ParseKey(bad)
		require.ErrorIs(t, err, ErrInvalidAPIKey, bad)
	}
}

func TestApiKeyStatusAndUsable(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	k := &ApiKey{}
	assert.Equal(t, KeyActive, k.Status(now))
	require.NoError(t, k.AssertUsable(now))

	future := now.Add(time.Hour)
	k.ExpiresAt = &future
	assert.Equal(t, KeyActive, k.Status(now))
	past := now.Add(-time.Second)
	k.ExpiresAt = &past
	assert.Equal(t, KeyExpired, k.Status(now))
	require.ErrorIs(t, k.AssertUsable(now), ErrAPIKeyExpired)

	assert.True(t, k.Revoke(now))
	assert.False(t, k.Revoke(now), "冪等")
	assert.Equal(t, KeyRevoked, k.Status(now), "revoked 優先於 expired")
	require.ErrorIs(t, k.AssertUsable(now), ErrAPIKeyRevoked)
}

func TestApiKeyScopes(t *testing.T) {
	k := &ApiKey{}
	assert.True(t, k.HasScope("payments:write"), "空 = 全部")
	k.Scopes = []string{"payments:read"}
	assert.True(t, k.HasScope("payments:read"))
	assert.False(t, k.HasScope("payments:write"))

	got, err := ValidateScopes([]string{"payments:read", "payments:read", "refunds:write"})
	require.NoError(t, err)
	assert.Equal(t, []string{"payments:read", "refunds:write"}, got)
	_, err = ValidateScopes([]string{"admin:*"})
	require.ErrorIs(t, err, ErrParameterInvalid)
}

func TestApiKeyRotateSigningSecret(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	k := &ApiKey{SigningSecretEnc: "enc-old"}
	k.RotateSigningSecret("enc-new", now, 24*time.Hour)
	assert.Equal(t, "enc-new", k.SigningSecretEnc)
	assert.Equal(t, "enc-old", k.PreviousSigningSecretEnc)
	assert.True(t, k.PreviousSecretValid(now.Add(23*time.Hour)))
	assert.False(t, k.PreviousSecretValid(now.Add(25*time.Hour)))
}

func BenchmarkVerifyArgon2id(b *testing.B) {
	plain, key, _ := GenerateKey(ModeLive)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key.Verify(plain)
	}
}

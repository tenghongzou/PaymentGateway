package domain

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Secret 前綴（docs/06 §3.1、§4.1）。
const (
	SigningSecretSchemeLive = "sk_live_"
	SigningSecretSchemeTest = "sk_test_"
	WebhookSecretScheme     = "whsec_"
)

var (
	signingSecretRe = regexp.MustCompile(`^sk_(live|test)_[0-9A-Za-z]{43}$`)
	webhookSecretRe = regexp.MustCompile(`^whsec_[0-9A-Za-z]{43}$`)
)

// GenerateSigningSecret 產生請求簽章 secret：sk_<mode>_ + 43 碼 base62。
func GenerateSigningSecret(mode Mode) string {
	if mode == ModeTest {
		return SigningSecretSchemeTest + randomBody()
	}
	return SigningSecretSchemeLive + randomBody()
}

// GenerateWebhookSecret 產生 Webhook 簽章 secret：whsec_ + 43 碼 base62。
func GenerateWebhookSecret() string { return WebhookSecretScheme + randomBody() }

// IsSigningSecret / IsWebhookSecret 檢查格式（測試與 seed 用）。
func IsSigningSecret(s string) bool { return signingSecretRe.MatchString(s) }

// IsWebhookSecret 檢查 whsec_ 格式。
func IsWebhookSecret(s string) bool { return webhookSecretRe.MatchString(s) }

// SecretCipher 封裝 signing secret / webhook secret 的欄位級加密。
//
// Phase 0：AES-256-GCM + 環境變數 PG_KEK（envelope-lite，沒有 per-record DEK）。
// Phase 1：改為 docs/06 §7.3 的 Vault transit envelope encryption（每筆 DEK、KEK 版本化、rewrap）。
// aad 依 06 §7.3 為 "{table}:{column}:{record_id}"，防止密文被搬到別筆記錄。
type SecretCipher interface {
	Encrypt(plaintext, aad string) (string, error)
	Decrypt(ciphertext, aad string) (string, error)
}

// 儲存格式前綴。
const (
	cipherPrefixAESGCM = "aesgcm:v1:"
	cipherPrefixPlain  = "plain:v1:"
)

// KEKLen 為 AES-256 金鑰長度。
const KEKLen = 32

var (
	// ErrKEKRequired 表示遇到 aesgcm 密文但沒有 KEK。
	ErrKEKRequired = errors.New("domain: ciphertext is encrypted but no KEK is configured")
	// ErrCiphertextInvalid 表示密文格式錯誤或驗證失敗。
	ErrCiphertextInvalid = errors.New("domain: ciphertext invalid")
)

// AESGCMCipher 為 AES-256-GCM 實作。
type AESGCMCipher struct {
	aead cipher.AEAD
}

// NewAESGCMCipher 以 32 bytes KEK 建立 cipher。
func NewAESGCMCipher(kek []byte) (*AESGCMCipher, error) {
	if len(kek) != KEKLen {
		return nil, fmt.Errorf("domain: KEK must be %d bytes, got %d", KEKLen, len(kek))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("domain: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("domain: gcm: %w", err)
	}
	return &AESGCMCipher{aead: aead}, nil
}

// NewAESGCMCipherFromBase64 以 base64（std 或 raw）編碼的 KEK 建立 cipher。
func NewAESGCMCipherFromBase64(encoded string) (*AESGCMCipher, error) {
	encoded = strings.TrimSpace(encoded)
	kek, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		kek, err = base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("domain: KEK must be base64: %w", err)
		}
	}
	return NewAESGCMCipher(kek)
}

// Encrypt 回傳 "aesgcm:v1:" + base64(nonce || ciphertext)。
func (c *AESGCMCipher) Encrypt(plaintext, aad string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	_, _ = rand.Read(nonce)
	sealed := c.aead.Seal(nil, nonce, []byte(plaintext), []byte(aad))
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return cipherPrefixAESGCM + base64.RawStdEncoding.EncodeToString(out), nil
}

// Decrypt 解密；為了讓「先用 plaintext 後來才設 KEK」的 dev 資料仍可讀，也接受 plain:v1: 格式。
func (c *AESGCMCipher) Decrypt(ciphertext, aad string) (string, error) {
	if strings.HasPrefix(ciphertext, cipherPrefixPlain) {
		return strings.TrimPrefix(ciphertext, cipherPrefixPlain), nil
	}
	if !strings.HasPrefix(ciphertext, cipherPrefixAESGCM) {
		return "", ErrCiphertextInvalid
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(ciphertext, cipherPrefixAESGCM))
	if err != nil || len(raw) < c.aead.NonceSize() {
		return "", ErrCiphertextInvalid
	}
	ns := c.aead.NonceSize()
	plain, err := c.aead.Open(nil, raw[:ns], raw[ns:], []byte(aad))
	if err != nil {
		return "", ErrCiphertextInvalid
	}
	return string(plain), nil
}

// PlaintextCipher 為 dev 沒有設定 PG_KEK 時的退路：以 "plain:v1:" 前綴標記明文存放。
// **絕不可用於 staging / production**；建構時由呼叫端（main）印出明確 warning。
type PlaintextCipher struct{}

// Encrypt 以 plain:v1: 前綴標記。
func (PlaintextCipher) Encrypt(plaintext, _ string) (string, error) {
	return cipherPrefixPlain + plaintext, nil
}

// Decrypt 只接受 plain:v1:；遇到 aesgcm 密文回 ErrKEKRequired。
func (PlaintextCipher) Decrypt(ciphertext, _ string) (string, error) {
	switch {
	case strings.HasPrefix(ciphertext, cipherPrefixPlain):
		return strings.TrimPrefix(ciphertext, cipherPrefixPlain), nil
	case strings.HasPrefix(ciphertext, cipherPrefixAESGCM):
		return "", ErrKEKRequired
	default:
		return "", ErrCiphertextInvalid
	}
}

// SecretAAD 組出 docs/06 §7.3 規定的 AAD："{table}:{column}:{record_id}"。
func SecretAAD(table, column, recordID string) string {
	return table + ":" + column + ":" + recordID
}

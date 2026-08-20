package domain

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// Mode 為 API Key 模式（live / test）。
type Mode string

// 模式全集。
const (
	ModeLive Mode = "live"
	ModeTest Mode = "test"
)

// ParseMode 解析 mode 字串。
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeLive, ModeTest:
		return Mode(s), nil
	default:
		return "", invalid("mode", "mode must be live or test")
	}
}

// Key 格式常數（docs/06 §3.1；docs/04 §1.2：prefix 為可顯示、可查詢的前 16 碼）。
const (
	apiKeyScheme = "pk_"
	// LookupIDLen 為 key 隨機部分前 8 碼（明文索引）。
	LookupIDLen = 8
	// APIKeyPrefixLen = len("pk_live_") + 8。
	APIKeyPrefixLen = 8 + LookupIDLen
	// APIKeyLen = len("pk_live_") + 43。
	APIKeyLen = 8 + secretBodyLen
)

var apiKeyRe = regexp.MustCompile(`^pk_(live|test)_[0-9A-Za-z]{43}$`)

// Argon2id 參數（docs/06 §3.2：m = 64 MiB、t = 3、p = 4、salt 16 bytes、tag 32 bytes）。
const (
	Argon2Memory  uint32 = 64 * 1024
	Argon2Time    uint32 = 3
	Argon2Threads uint8  = 4
	Argon2SaltLen        = 16
	Argon2KeyLen  uint32 = 32
)

// 已知 scopes（proto ApiKey.scopes 註解 + docs/06 §3.1 的 balance:read）。空陣列表示全部。
var knownScopes = map[string]struct{}{
	"payments:read": {}, "payments:write": {},
	"refunds:read": {}, "refunds:write": {},
	"disputes:read": {}, "disputes:write": {},
	"webhooks:manage": {}, "api_keys:manage": {},
	"ledger:read": {}, "events:read": {}, "balance:read": {},
}

// KnownScopes 回傳已知 scope 清單（排序後）。
func KnownScopes() []string {
	out := make([]string, 0, len(knownScopes))
	for s := range knownScopes {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// ValidateScopes 檢查 scopes 皆為已知值；回傳去重後的清單。
func ValidateScopes(scopes []string) ([]string, error) {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if _, ok := knownScopes[s]; !ok {
			return nil, invalid("scopes", "unknown scope %q", s)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// KeyStatus 為 key 的衍生狀態（DB 沒有 status 欄位，由 revoked_at / expires_at 推導）。
type KeyStatus string

// KeyStatus 全集。
const (
	KeyActive  KeyStatus = "active"
	KeyRevoked KeyStatus = "revoked"
	KeyExpired KeyStatus = "expired"
)

// ApiKey 為商戶 API Key 實體（docs/02 §2.1）。
//
// Phase 0 的 signing secret 存在 api_keys.metadata jsonb（_signing_secret_enc 等內部鍵），
// TODO：補 migration 加 signing_secret_enc / previous_signing_secret_enc / previous_secret_expires_at 專欄。
type ApiKey struct { //nolint:revive // 名稱與 proto / docs 一致
	ID         uuid.UUID
	MerchantID uuid.UUID
	// Prefix 為 pk_live_ + lookup_id（8 碼），對應 api_keys.prefix（UNIQUE）。
	Prefix string
	// KeyHash 為 Argon2id PHC 字串。
	KeyHash string
	Mode    Mode
	Name    string
	Scopes  []string
	// SigningSecretEnc 為加密後的 sk_ secret（SecretCipher 產生）。
	SigningSecretEnc string
	// PreviousSigningSecretEnc / PreviousSecretExpiresAt 為輪替視窗內仍接受的上一把 secret。
	PreviousSigningSecretEnc string
	PreviousSecretExpiresAt  *time.Time
	LastUsedAt               *time.Time
	ExpiresAt                *time.Time
	RevokedAt                *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// PublicID 回傳對外 ID（key_ + base32(uuid)）。
func (k *ApiKey) PublicID() string { return ids.Format(ids.PrefixAPIKey, k.ID) }

// ParseAPIKeyID 解析 key_... 為 uuid。
func ParseAPIKeyID(publicID string) (uuid.UUID, error) {
	if publicID == "" {
		return uuid.Nil, missing("api_key_id")
	}
	u, err := ids.ParseWithPrefix(publicID, ids.PrefixAPIKey)
	if err != nil {
		return uuid.Nil, invalid("api_key_id", "api_key_id must look like key_<26 chars>")
	}
	return u, nil
}

// LookupID 回傳 prefix 的隨機部分（8 碼）。
func (k *ApiKey) LookupID() string {
	if len(k.Prefix) < APIKeyPrefixLen {
		return ""
	}
	return k.Prefix[APIKeyPrefixLen-LookupIDLen:]
}

// Status 回傳衍生狀態。
func (k *ApiKey) Status(now time.Time) KeyStatus {
	if k.RevokedAt != nil {
		return KeyRevoked
	}
	if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
		return KeyExpired
	}
	return KeyActive
}

// AssertUsable 檢查 key 是否可用：revoked → ErrAPIKeyRevoked，expired → ErrAPIKeyExpired。
func (k *ApiKey) AssertUsable(now time.Time) error {
	switch k.Status(now) {
	case KeyRevoked:
		return ErrAPIKeyRevoked
	case KeyExpired:
		return ErrAPIKeyExpired
	case KeyActive:
		return nil
	default:
		return nil
	}
}

// Revoke 撤銷 key（冪等；回傳是否有實際變更）。
func (k *ApiKey) Revoke(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	t := now
	k.RevokedAt = &t
	return true
}

// HasScope 檢查 key 是否具備 scope（空 scopes = 全部）。
func (k *ApiKey) HasScope(scope string) bool {
	return len(k.Scopes) == 0 || slices.Contains(k.Scopes, scope)
}

// RotateSigningSecret 把目前 secret 移到 previous（grace 視窗內仍接受）並設定新 secret。
func (k *ApiKey) RotateSigningSecret(newEnc string, now time.Time, grace time.Duration) {
	if k.SigningSecretEnc != "" {
		exp := now.Add(grace)
		k.PreviousSigningSecretEnc = k.SigningSecretEnc
		k.PreviousSecretExpiresAt = &exp
	}
	k.SigningSecretEnc = newEnc
}

// PreviousSecretValid 回傳是否仍在輪替視窗內。
func (k *ApiKey) PreviousSecretValid(now time.Time) bool {
	return k.PreviousSigningSecretEnc != "" && k.PreviousSecretExpiresAt != nil && k.PreviousSecretExpiresAt.After(now)
}

// Verify 以 Argon2id 比對明文 key（常數時間比較）。格式錯誤或 hash 損毀一律回 false。
func (k *ApiKey) Verify(plaintext string) bool {
	return VerifyArgon2id(k.KeyHash, plaintext)
}

// GenerateKey 產生新的 API Key：回傳明文（只在此刻存在）與只含 hash 的實體。
// 呼叫端需再填入 MerchantID / Name / Scopes / ExpiresAt / SigningSecretEnc / CreatedAt。
func GenerateKey(mode Mode) (plaintext string, key *ApiKey, err error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return "", nil, err
	}
	body := randomBody()
	plaintext = apiKeyScheme + string(mode) + "_" + body
	key = &ApiKey{
		ID:      ids.NewUUID(),
		Prefix:  plaintext[:APIKeyPrefixLen],
		KeyHash: HashArgon2id(plaintext),
		Mode:    mode,
	}
	return plaintext, key, nil
}

// ParseKey 檢查明文 key 格式並回傳 mode 與 prefix（前 16 碼）；格式不符回 ErrInvalidAPIKey。
func ParseKey(plaintext string) (mode Mode, prefix string, err error) {
	if !apiKeyRe.MatchString(plaintext) {
		return "", "", ErrInvalidAPIKey
	}
	return Mode(plaintext[3:7]), plaintext[:APIKeyPrefixLen], nil
}

// HashArgon2id 以 docs/06 §3.2 參數產生 PHC 字串：$argon2id$v=19$m=65536,t=3,p=4$<salt_b64>$<hash_b64>。
func HashArgon2id(plaintext string) string {
	salt := make([]byte, Argon2SaltLen)
	_, _ = rand.Read(salt)
	return hashArgon2idWithSalt(plaintext, salt)
}

func hashArgon2idWithSalt(plaintext string, salt []byte) string {
	sum := argon2.IDKey([]byte(plaintext), salt, Argon2Time, Argon2Memory, Argon2Threads, Argon2KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, Argon2Memory, Argon2Time, Argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum))
}

// argon2Params 為從 PHC 字串解析出的參數。
type argon2Params struct {
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	hash    []byte
}

var errBadPHC = errors.New("domain: malformed argon2id hash")

// parseArgon2id 解析 PHC 字串（參數從字串讀取，以便未來調整參數後舊 hash 仍可驗證）。
func parseArgon2id(encoded string) (*argon2Params, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, errBadPHC
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return nil, errBadPHC
	}
	p := &argon2Params{}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return nil, errBadPHC
	}
	var err error
	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) == 0 {
		return nil, errBadPHC
	}
	if p.hash, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.hash) == 0 {
		return nil, errBadPHC
	}
	return p, nil
}

// VerifyArgon2id 驗證明文是否符合 PHC hash（常數時間比較）。
func VerifyArgon2id(encoded, plaintext string) bool {
	p, err := parseArgon2id(encoded)
	if err != nil {
		return false
	}
	sum := argon2.IDKey([]byte(plaintext), p.salt, p.time, p.memory, p.threads, uint32(len(p.hash))) //nolint:gosec // len(hash) ≤ 32
	return subtle.ConstantTimeCompare(sum, p.hash) == 1
}

package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// CreateApiKeyInput 對應 CreateApiKeyRequest。
type CreateApiKeyInput struct { //nolint:revive // 與 proto 命名一致
	MerchantID string
	Mode       domain.Mode
	Name       string
	Scopes     []string
	ExpiresAt  *time.Time
}

// CreateApiKeyOutput 含只回傳一次的明文 key 與 signing secret。
type CreateApiKeyOutput struct { //nolint:revive // 與 proto 命名一致
	Key           *domain.ApiKey
	Plaintext     string
	SigningSecret string
}

// CreateApiKey 建立 API Key：Argon2id hash 入庫、signing secret 加密入庫、同交易寫 outbox。
func (s *Service) CreateApiKey(ctx context.Context, in CreateApiKeyInput) (*CreateApiKeyOutput, error) {
	if _, err := domain.ParseMode(string(in.Mode)); err != nil {
		return nil, err
	}
	if err := domain.ValidateName(in.Name); err != nil {
		return nil, err
	}
	scopes, err := domain.ValidateScopes(in.Scopes)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		return nil, domain.ErrParameterInvalid.WithParam("expires_at").WithMessage("expires_at must be in the future")
	}
	var out *CreateApiKeyOutput
	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		m, err := s.lockMerchant(ctx, in.MerchantID)
		if err != nil {
			return err
		}
		if err := m.AssertWritable(); err != nil {
			return err
		}
		n, err := s.keys.CountActive(ctx, m.ID, in.Mode, now)
		if err != nil {
			return fmt.Errorf("app: count api keys: %w", err)
		}
		if n >= s.cfg.MaxAPIKeysPerMode {
			return domain.ErrAPIKeyLimit.WithMessage("merchant already has %d active %s keys", n, in.Mode)
		}
		plaintext, key, err := domain.GenerateKey(in.Mode)
		if err != nil {
			return err
		}
		key.MerchantID = m.ID
		key.Name = strings.TrimSpace(in.Name)
		key.Scopes = scopes
		key.ExpiresAt = in.ExpiresAt
		key.CreatedAt = now
		key.UpdatedAt = now
		secret := domain.GenerateSigningSecret(in.Mode)
		enc, err := s.cipher.Encrypt(secret, keyAAD(key.ID, "signing_secret_enc"))
		if err != nil {
			return fmt.Errorf("app: encrypt signing secret: %w", err)
		}
		key.SigningSecretEnc = enc
		if err := s.keys.Create(ctx, key); err != nil {
			return fmt.Errorf("app: create api key: %w", err)
		}
		out = &CreateApiKeyOutput{Key: key, Plaintext: plaintext, SigningSecret: secret}
		return s.emit(ctx, AggregateAPIKey, EventAPIKeyCreated, m.PublicID(), apiKeyEventData(key, now, ""))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RevokeApiKey 撤銷 key（冪等）；發佈 api_key.revoked 讓 gateway 清快取。
func (s *Service) RevokeApiKey(ctx context.Context, merchantID, apiKeyID, reason string) (*domain.ApiKey, error) {
	mid, err := domain.ParseMerchantID(merchantID)
	if err != nil {
		return nil, err
	}
	kid, err := domain.ParseAPIKeyID(apiKeyID)
	if err != nil {
		return nil, err
	}
	var out *domain.ApiKey
	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		key, err := s.keys.Get(ctx, mid, kid)
		if err != nil {
			return err
		}
		out = key
		now := s.clock.Now()
		if !key.Revoke(now) {
			return nil // 已撤銷：冪等成功，不再發事件
		}
		key.UpdatedAt = now
		if err := s.keys.Update(ctx, key); err != nil {
			return fmt.Errorf("app: revoke api key: %w", err)
		}
		s.lastUsed.Delete(key.ID)
		return s.emit(ctx, AggregateAPIKey, EventAPIKeyRevoked, merchantID, apiKeyEventData(key, now, reason))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListApiKeysInput 對應 ListApiKeysRequest。
type ListApiKeysInput struct { //nolint:revive // 與 proto 命名一致
	MerchantID      string
	Mode            domain.Mode
	IncludeInactive bool
	Page            Page
}

// ListApiKeys 列出商戶的 key（不含 hash / secret）。
func (s *Service) ListApiKeys(ctx context.Context, in ListApiKeysInput) ([]*domain.ApiKey, string, error) {
	mid, err := domain.ParseMerchantID(in.MerchantID)
	if err != nil {
		return nil, "", err
	}
	if in.Mode != "" {
		if _, err := domain.ParseMode(string(in.Mode)); err != nil {
			return nil, "", err
		}
	}
	return s.keys.List(ctx, mid, ApiKeyFilter{Mode: in.Mode, IncludeInactive: in.IncludeInactive}, in.Page.Normalize())
}

// VerifyApiKey 的無效原因（proto VerifyApiKeyResponse.reason）。
const (
	ReasonNotFound       = "not_found"
	ReasonRevoked        = "revoked"
	ReasonExpired        = "expired"
	ReasonMerchantClosed = "merchant_closed"
)

// VerifyApiKeyResult 為驗證結果；Valid=false 時只有 Reason 有值。
type VerifyApiKeyResult struct { //nolint:revive // 與 proto 命名一致
	Valid  bool
	Reason string
	Key    *domain.ApiKey
	// Merchant 含狀態（suspended 時 Valid 仍為 true，由 gateway 依操作種類決定）。
	Merchant *domain.Merchant
	// SigningSecret / PreviousSigningSecret 為解密後明文；gateway 只可放行程內記憶體快取。
	SigningSecret         string
	PreviousSigningSecret string
}

// VerifyApiKey 驗證明文 key（docs/06 §3.3 步驟 2–5）：
//
//	格式檢查 → 以 prefix 查候選（≤2）→ Argon2id verify → revoked / expired → 商戶 closed → 解密 signing secret。
//
// 任何「無效」都回 Valid=false + Reason，不回 error（error 只代表系統故障）。
// 找不到候選時仍執行一次 Argon2id，讓回應時間與比對失敗一致，避免列舉。
func (s *Service) VerifyApiKey(ctx context.Context, plaintext string) (*VerifyApiKeyResult, error) {
	_, prefix, err := domain.ParseKey(plaintext)
	if err != nil {
		s.burnArgon2(plaintext)
		return &VerifyApiKeyResult{Reason: ReasonNotFound}, nil
	}
	candidates, err := s.keys.FindByPrefix(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("app: find api key: %w", err)
	}
	var match *domain.ApiKey
	for _, c := range candidates {
		if c.Verify(plaintext) {
			match = c
			break
		}
	}
	if match == nil {
		if len(candidates) == 0 {
			s.burnArgon2(plaintext)
		}
		return &VerifyApiKeyResult{Reason: ReasonNotFound}, nil
	}
	now := s.clock.Now()
	switch match.Status(now) {
	case domain.KeyRevoked:
		return &VerifyApiKeyResult{Reason: ReasonRevoked}, nil
	case domain.KeyExpired:
		return &VerifyApiKeyResult{Reason: ReasonExpired}, nil
	case domain.KeyActive:
	}
	m, err := s.merchants.Get(ctx, match.MerchantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return &VerifyApiKeyResult{Reason: ReasonNotFound}, nil
		}
		return nil, fmt.Errorf("app: load merchant for api key: %w", err)
	}
	if m.Status == domain.StatusClosed {
		return &VerifyApiKeyResult{Reason: ReasonMerchantClosed}, nil
	}
	secret, err := s.cipher.Decrypt(match.SigningSecretEnc, keyAAD(match.ID, "signing_secret_enc"))
	if err != nil {
		return nil, domain.ErrSecretUnavailable.Wrap(fmt.Errorf("key %s: %w", match.PublicID(), err))
	}
	res := &VerifyApiKeyResult{Valid: true, Key: match, Merchant: m, SigningSecret: secret}
	if match.PreviousSecretValid(now) {
		prev, err := s.cipher.Decrypt(match.PreviousSigningSecretEnc, keyAAD(match.ID, "previous_signing_secret_enc"))
		if err != nil {
			// 舊 secret 解不開不影響主流程，記 log 後略過。
			s.log.WarnContext(ctx, "previous signing secret undecryptable", "api_key_id", match.PublicID(), "err", err)
		} else {
			res.PreviousSigningSecret = prev
		}
	}
	s.touchLastUsed(ctx, match.ID, now)
	return res, nil
}

// burnArgon2 對 dummy hash 做一次驗證（固定成本）。
func (s *Service) burnArgon2(plaintext string) {
	_ = domain.VerifyArgon2id(s.dummyHash, plaintext)
}

// touchLastUsed 以每把 key 最多每 LastUsedInterval 一次的頻率更新 last_used_at（預設非同步）。
func (s *Service) touchLastUsed(ctx context.Context, id uuid.UUID, now time.Time) {
	if prev, ok := s.lastUsed.Load(id); ok {
		if t, _ := prev.(time.Time); now.Sub(t) < s.cfg.LastUsedInterval {
			return
		}
	}
	s.lastUsed.Store(id, now)
	if s.cfg.SyncLastUsed {
		if err := s.keys.TouchLastUsed(ctx, id, now); err != nil {
			s.log.WarnContext(ctx, "touch last_used_at failed", "api_key_id", id, "err", err)
		}
		return
	}
	s.touchWG.Add(1)
	go func() {
		defer s.touchWG.Done()
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.keys.TouchLastUsed(bg, id, now); err != nil {
			s.log.WarnContext(bg, "touch last_used_at failed", "api_key_id", id, "err", err)
		}
	}()
}

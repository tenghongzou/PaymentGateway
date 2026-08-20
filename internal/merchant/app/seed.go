package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/tenghongzou/paymentgateway/internal/merchant/domain"
)

// SeedDevExternalRef 為 dev seed 商戶的 external_ref（重複執行會重用同一商戶、另發一把新 key）。
const SeedDevExternalRef = "dev-seed"

// SeedDevResult 為 seed-dev 子命令的輸出（供 api-gateway 開發模式使用）。
type SeedDevResult struct {
	MerchantID    string
	APIKeyID      string
	APIKey        string
	SigningSecret string
	Reused        bool
}

// SeedDev 建立（或重用）一個 dev 商戶並簽發一把 test key + signing secret。
func (s *Service) SeedDev(ctx context.Context) (*SeedDevResult, error) {
	m, err := s.merchants.FindByExternalRef(ctx, SeedDevExternalRef)
	reused := true
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("app: seed lookup: %w", err)
		}
		reused = false
		m, err = s.CreateMerchant(ctx, CreateMerchantInput{
			Name: "Dev Merchant", LegalName: "Dev Merchant Co., Ltd.", Country: "TW", DefaultCurrency: "TWD",
			ContactEmail: "dev@example.com", StatementDescriptor: "PG*DEV", ExternalRef: SeedDevExternalRef,
			Metadata: map[string]string{"seed": "dev"},
		})
		if err != nil {
			return nil, fmt.Errorf("app: seed merchant: %w", err)
		}
	}
	key, err := s.CreateApiKey(ctx, CreateApiKeyInput{MerchantID: m.PublicID(), Mode: domain.ModeTest, Name: "dev seed key"})
	if err != nil {
		return nil, fmt.Errorf("app: seed api key: %w", err)
	}
	return &SeedDevResult{
		MerchantID: m.PublicID(), APIKeyID: key.Key.PublicID(), APIKey: key.Plaintext, SigningSecret: key.SigningSecret, Reused: reused,
	}, nil
}

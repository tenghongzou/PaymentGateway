// Package merchant 為 merchant-service 的程式碼根目錄（商戶、API Key（Argon2id hash）、Webhook 端點、路由偏好）。
//
// 三層結構（docs/01 §7）：
//
//	internal/merchant/
//	├── domain/            實體、值物件、狀態機、領域錯誤；Argon2id、AES-256-GCM 欄位加密、URL SSRF 檢查（只 import 標準庫與 pkg/）
//	├── app/               ports（MerchantRepo / ApiKeyRepo / WebhookEndpointRepo / RoutingPrefRepo / OutboxStore / TxManager / Clock）
//	│   │                  與對應 14 個 rpc 的 use cases；寫入皆在交易內並同交易寫 outbox（topic merchant.events，JSON payload）
//	│   └── apptest/       ports 的記憶體 fake（app / adapter 單元測試用）
//	└── adapter/
//	    ├── grpc/          pg.merchant.v1.MerchantService 實作（錯誤經 pkg/grpcx.ErrorFromDomain）
//	    └── postgres/      repository 實作（SQL 對齊 migrations/merchant；`-tags integration` 以 testcontainers 驗證）
//
// Phase 0 已知權宜（皆已在程式註解標 TODO，待補 migration）：
//   - api_keys 無 signing_secret 專欄 → 存於 metadata jsonb 內部鍵（_signing_secret_enc 等）
//   - webhook_endpoints 無 mode / deleted_at 專欄、status CHECK 只有 enabled|disabled → metadata 內部鍵（_mode/_deleted_at/_auto_disabled）
//   - routing_preferences 只有 rules → failover_enabled / max_attempts / fallback_providers 存於 merchants.settings
//   - merchants 無 legal_name / contact_email / external_ref 專欄 → 存於 settings（external_ref 唯一性由應用層檢查）
//   - secret 欄位加密為 AES-256-GCM + PG_KEK（envelope-lite）；Phase 1 改 Vault transit（docs/06 §7.3）
package merchant

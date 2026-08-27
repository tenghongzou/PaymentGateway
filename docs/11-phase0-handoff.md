# Phase 0 交接文件（2026-08-21）

> 本文件彙整 Phase 0（設計 + 可編譯骨架 + provider-mock 端到端）的交付狀態、已知落差與 Phase 1 backlog。
> 架構決策仍以 `01-architecture.md` 與 `docs/adr/` 為準。

## 1. 交付狀態

| 項目 | 狀態 | 驗證 |
|---|---|---|
| 設計文件 01–10、ADR 0001–0012 | ✅ 完成並交叉對齊（欄位名以 SQL 為準、狀態集統一、HMAC canonical 統一） | 人工交叉審查 |
| Protobuf（7 個 service、49 rpc）+ 產物 `api/gen/go` | ✅ | `buf lint` / `buf build` 通過 |
| OpenAPI 3.1（27 operation、14 webhook 事件） | ✅ | — |
| Migrations（5 個 DB） | ✅ | PostgreSQL 16 容器 up/down/不變條件測試通過 |
| 部署：Dockerfile、compose、Helm、CI、OTel、告警 | ✅ | `docker compose config`、`helm lint/template` 通過 |
| `pkg/*` 14 個共用套件 | ✅ | 單元測試、lint 0 issue |
| payment-service（狀態機、failover、outbox、gRPC） | ✅ 核心流程 | 單元 + 整合（testcontainers）+ e2e 7 案例；lint 0 |
| api-gateway（驗簽、冪等、限流、REST） | ✅ | handler 測試 + e2e；lint 0 |
| provider-mock（tok_* 情境） | ✅ | 單元測試；lint 0 |
| merchant-service | ✅ 14 rpc 全實作 | 單元 + 整合 + smoke；lint 0 |
| ledger-service | ✅ 7 rpc + consumer + 分錄範本 | 單元 + property-based + 整合；lint 0 |
| webhook-service | ✅ 4 rpc + dispatcher + reaper + dev sink | 單元 + 整合 + race + smoke；lint 0 |
| reconciliation-service | ✅ 5 rpc + matcher + consumer | 單元 + 整合；lint 0 |
| provider-stripe | ⏳ 骨架（HealthCheck NOT_SERVING） | Phase 1 |

最終整合驗證（2026-08-21）：`go build ./...` ✅、`go vet ./...` ✅、`go test ./...` 36 套件全過 ✅、`make build` 8 個 binary ✅、服務間 import 隔離（`go list -deps`）✅。

收尾驗證（2026-08-27，lint 清零後重跑）：`make build` 8 binary ✅、`go test ./...` 36 套件 ✅、各服務 `-tags integration`（testcontainers）✅、ledger/webhook 含 `-race` ✅、`buf lint`/`buf build` ✅、`compose config` ✅、全 repo `golangci-lint run ./...` **0 issues** ✅、compose 全 stack e2e 7 案例（Healthz / CreateGetRefund / HardDecline / UnavailableOnceRecovers / ManualCaptureAndVoid / 3DSConfirm / AuthRejected）✅。

## 2. 立即待辦（Phase 0 收尾）— ✅ 已完成（2026-08-27）

### 2.1 Lint 清理 — 完成
四個第二波服務的 lint issue（交接時記 412 條、收尾時實測 402 條：merchant 59、ledger 96、webhook 120、reconciliation 127）已全數清零；全 repo `./bin/golangci-lint run ./...` 為 **0 issues**。遵循原則：未改 `.golangci.yaml`；`exhaustive` 一律補實際 case（含 `internal/ledger/app/proto.go` 的 `domain.Kind` 映射涵蓋 10 個科目）；`errcheck` 真正處理錯誤；`depguard`（ledger property test 的 `math/rand`）以遷移至 `math/rand/v2` 修復。僅保留少量附理由的單行 `//nolint`：protoc 產生的 gRPC 介面方法名（revive var-naming）、gosec 對 SQL 欄位清單的 G101 誤判與有上界的 G115 轉型、forbidigo「金額禁 float」規則誤中 webhook 退避 jitter 亂數與 jsonb 數字型別還原、文件化契約的 nilerr（`VerifyApiKey` 無效 key 回 `Valid=false` 不回 error）。

### 2.2 文件小修 — 完成
- `02 §0.2` ID 前綴已依 SQL 改為 `re_/dp_/we_`。
- `05 §7.2 #11` 與 `02 附錄 B` 已以 OpenAPI 為準改為 14 種事件全部推送（與 `internal/webhook/domain/event.go` 實作一致）。

## 3. Schema backlog（Phase 1 第一個 migration）
程式目前以既有欄位（多為 `metadata`/`settings` jsonb）安全落地，以下欄位化後可移除對應 workaround：

| DB | 變更 | 目前 workaround |
|---|---|---|
| merchant | `api_keys.signing_secret_enc`、`previous_signing_secret_enc`、`previous_expires_at` | `api_keys.metadata._signing_secret_enc` 等 |
| merchant | `webhook_endpoints.mode`、`deleted_at`；status CHECK 加 `deleted` | metadata `_mode/_deleted_at/_auto_disabled` |
| merchant | `merchants.external_ref UNIQUE`、`legal_name`、`contact_email` | `settings` + 應用層唯一檢查 |
| payment | `payments.livemode`、`refunds.livemode` | gateway 以 API key mode 回填 |
| payment | `payment_attempts.public_id` | `att_` 由 uuid 推導 |
| ledger | `accounts.livemode` 並納入唯一鍵 | test 模式帳戶 code 前綴 `test:` |
| ledger | `journals.effective_at/source_type/livemode`、`entries.description`、`journals.merchant_id` 允許 NULL | `journals.metadata`；系統 journal 以 `uuid.Nil` |
| reconciliation | `discrepancies.kind` CHECK 加 `fee_mismatch` | `amount_mismatch` + `details.kind` |
| reconciliation | `payment_records.fee`、`settlement_lines.fee`、`reconciliation_runs.file_id` | `raw.fee_minor`、`summary.file_id` |

## 4. Proto backlog
- `PaymentService.IngestProviderWebhook`（PSP inbound webhook 目前在 gateway 驗簽後只記 log）。
- `LedgerEvent` message（outbox 暫用 `pg.ledger.v1.Journal`）、`ReconciliationEvent`（暫用 JSON）。
- `DisputeOpened.stage`（inquiry 不記帳）。
- `MerchantService.RotateSigningSecret`。
- `PaymentEvent` 加 `request_id`。

## 5. 功能 TODO（依服務）

**payment-service**：expire / auth-expiry sweeper、operation-reconciler（capture/void 逾時收斂）、refund-reconciler、`ListRefunds / GetDispute / ListDisputes / SubmitDisputeEvidence`、Redis 滑動視窗 circuit breaker（目前單實例記憶體版）、`PG_PROVIDER_RETRY_SAME_ON_UNAVAILABLE` 預設值於多 provider 時應改 false。

**api-gateway**：簽章 canonical 無 nonce——同一商戶同一秒送出完全相同請求會被 `signature_replayed` 擋下（API 設計師評估加 `X-Nonce`）；merchant.events 驅動的 API key 快取失效。

**provider-mock**：docs/09 §3.3 控制 API（Complete3DS / EmitWebhook / SetGlobalBehavior）、`PG_MOCK_FAILURE_RATE`、`x-mock-*` metadata。

**merchant-service**：`SecretCipher` 換 Vault transit；`ListWebhookEndpoints(include_secrets)` 的呼叫端身分檢查（mTLS）。

**ledger-service**：`aggregate_version` 舊事件丟棄、`refund.succeeded` 早於 `refund.created` 的 deferred 處理、`RefundSucceeded.fee` 歸屬（商戶費 vs PSP 成本）確認。

**webhook-service**：dead_letter 時寫 outbox `webhook.delivery.dead_lettered`、72h 全失敗自動停用、每端點 in-flight 上限、`RetryDelivery` 冪等鍵改 Redis/DB、thin payload、指標。

**reconciliation-service**：Stripe parser 完整欄位、`s3://` 來源、`ResolveDiscrepancy` 的 `ADJUST_LEDGER/RESYNC_PAYMENT`、自動 re-match、`ledger_postings` 讀模型。

**provider-stripe**：完整 adapter（Phase 1 主目標）。

## 6. 本機跑起來
```bash
make tools && make proto && make build
docker compose -f deploy/compose/docker-compose.yaml up -d postgres redis kafka
# 建 dev 商戶與金鑰
eval "$(PG_ENV=dev PG_DATABASE_URL='postgres://merchant_owner:merchant_owner@localhost:5432/pg_merchant?sslmode=disable' ./bin/merchant-service seed-dev 2>/dev/null)"
# 或用 PG_DEV_* 直接給 gateway（見 deploy/compose/.env.example）
./bin/provider-mock & ./bin/payment-service & ./bin/api-gateway &
scripts/dev-pay.sh                       # 建立 tok_ok 付款並查詢
go test -tags e2e ./test/e2e/...         # 7 個端到端案例
./bin/webhook-service sink -addr :8099 -secret whsec_x   # 本機收 webhook 並驗簽
```

# PaymentGateway — 系統架構與設計決策（Design Brief）

> 本文件是全隊的「單一事實來源」。所有服務、API、資料模型、部署設定都必須遵守這裡的決策。
> 若要變更決策，請新增 ADR（`docs/adr/`）而不是直接改本文件。

## 1. 目標與範圍

建立一套**多商戶（multi-tenant）支付閘道**，提供統一 API 讓商戶接入多家支付供應商（PSP：Stripe、Adyen、LINE Pay、ECPay…），並負責：

- 付款（Payment）、退款（Refund）、爭議/拒付（Chargeback）生命週期管理
- 供應商路由（Routing）與容錯切換（Failover）
- 雙式記帳帳本（Double-entry Ledger）與商戶餘額
- 對商戶的 Webhook 通知（簽章、重試、去重）
- 與 PSP 結算檔的對帳（Reconciliation）
- 商戶、API Key、Webhook 端點管理

**非目標（v1 不做）**：自行儲存卡號（PAN）、發卡、收單牌照相關功能、多幣別自動換匯。

## 2. 核心非功能需求

| 面向 | 要求 |
|---|---|
| 正確性 | 金額絕不使用浮點數；所有寫入操作冪等；帳本不可變（append-only） |
| 可用性 | 99.95%；單一 PSP 故障不影響整體（自動 failover） |
| 延遲 | 建立付款 P99 < 300ms（不含 PSP 往返） |
| 吞吐 | 初期 500 TPS，水平擴展至 5k TPS |
| 安全 | PCI-DSS SAQ-A / A-EP 範圍：**本系統不接觸 PAN**，使用 PSP 的 tokenization / hosted fields |
| 稽核 | 所有狀態轉移都留事件（Event Sourcing-lite：狀態表 + 事件表） |
| 可觀測 | OpenTelemetry traces/metrics/logs，trace id 貫穿 HTTP → gRPC → Kafka |

## 3. 技術棧（固定）

| 類別 | 選擇 | 備註 |
|---|---|---|
| 語言 | Go 1.26 | 單一 `go.mod` monorepo |
| Module path | `github.com/tenghongzou/paymentgateway` | |
| 對外 API | REST/JSON（`api-gateway`），OpenAPI 3.1 | 由 gRPC 服務組合 |
| 服務間同步通訊 | gRPC + Protobuf（`google.golang.org/grpc`） | mTLS（生產） |
| 服務間非同步 | Kafka（事件），**Transactional Outbox** 模式 | 事件 payload 亦用 Protobuf；client 使用 `github.com/twmb/franz-go` |
| 資料庫 | PostgreSQL 16，**database-per-service** | `pgx/v5`，migrations 用 `golang-migrate` 純 SQL |
| 快取 / 冪等 / 限流 | Valkey 8 | |
| 祕密管理 | HashiCorp Vault（生產）/ env（本機） | PSP API key、webhook signing secret |
| 可觀測性 | OpenTelemetry SDK → OTel Collector → Prometheus / Grafana / Jaeger / Loki | |
| HTTP router | `github.com/go-chi/chi/v5` | 只在 api-gateway |
| 設定 | 環境變數（`PG_` 前綴），`caarlos0/env` | 12-factor |
| Log | `log/slog` JSON | |
| 測試 | `testing` + `testcontainers-go`（整合）+ `stretchr/testify` | |
| 容器 / 編排 | Docker、docker-compose（本機）、Kubernetes + Helm（生產） | |
| CI | GitHub Actions：lint（golangci-lint）、test、build、proto breaking check | |

## 4. 服務切分（Bounded Contexts）

```
                         ┌──────────────────────┐
   Merchant backend ───▶ │     api-gateway      │  REST :8080   (auth, rate-limit, idempotency, REST→gRPC)
                         └──────────┬───────────┘
        ┌──────────────┬────────────┼──────────────┬─────────────────┐
        ▼              ▼            ▼              ▼                 ▼
 ┌────────────┐ ┌────────────┐ ┌──────────┐ ┌────────────┐ ┌──────────────────┐
 │ merchant   │ │ payment    │ │ ledger   │ │ webhook    │ │ reconciliation   │
 │ :9001      │ │ :9002      │ │ :9003    │ │ :9004      │ │ :9005            │
 └────────────┘ └─────┬──────┘ └──────────┘ └────────────┘ └──────────────────┘
                      │ gRPC (ProviderAdapter interface)
          ┌───────────┴───────────┐
          ▼                       ▼
   ┌──────────────┐        ┌──────────────┐
   │ provider-    │        │ provider-    │   每個 PSP 一個 adapter service，
   │ mock :9101   │        │ stripe :9102 │   實作同一份 ProviderAdapter proto
   └──────────────┘        └──────────────┘
                                   │
                          ◀── PSP webhooks（/psp/{provider}/webhook 經 api-gateway 進入）

   ─────────────── Kafka topics（事件流）───────────────
   payment.events / refund.events / ledger.events / merchant.events / reconciliation.events
```

### 4.1 各服務職責

| 服務 | gRPC port | DB | 職責 |
|---|---|---|---|
| `api-gateway` | HTTP 8080 | 無（Valkey） | API Key/HMAC 驗證、`Idempotency-Key` 處理、限流、REST↔gRPC 轉譯、PSP inbound webhook 入口 |
| `merchant-service` | 9001 | `pg_merchant` | 商戶、API Key（hash 儲存）、Webhook 端點、路由偏好設定 |
| `payment-service` | 9002 | `pg_payment` | Payment / Refund / Chargeback 聚合根與狀態機、路由與 failover、呼叫 provider adapter、發佈事件 |
| `ledger-service` | 9003 | `pg_ledger` | 雙式記帳：帳戶、日記帳（journal）、分錄（entries）、餘額；消費 payment 事件記帳 |
| `webhook-service` | 9004 | `pg_webhook` | 消費事件 → 產生對商戶的通知，HMAC-SHA256 簽章、指數退避重試、死信 |
| `reconciliation-service` | 9005 | `pg_recon` | 匯入 PSP 結算檔、與帳本/付款比對、差異報表 |
| `provider-mock` | 9101 | 無 | 模擬 PSP（可設定成功/失敗/延遲/3DS），開發與測試用 |
| `provider-stripe` | 9102 | 無 | Stripe adapter（v1 第一個真實 PSP） |
| `risk-service`（Phase 2） | 9006 | `pg_risk` | 規則引擎、velocity check、黑名單 |

### 4.2 供應商抽象（ProviderAdapter）

所有 PSP adapter 實作同一個 gRPC 介面 `pg.provider.v1.ProviderAdapter`：

`Authorize`、`Capture`、`Void`、`Refund`、`GetPaymentStatus`、`ParseWebhook`（把 PSP 原生 webhook 正規化成標準事件）、`HealthCheck`。

payment-service 只認識這個介面，不認識任何 PSP 細節。

## 5. 核心領域模型（摘要；細節見 `02-domain-and-ledger.md`）

### 5.1 Money
- `amount int64`（最小貨幣單位，例如 TWD 100 = 100、USD 1.00 = 100）
- `currency string`（ISO 4217，大寫 3 碼）
- 共用套件：`pkg/money`，禁止直接運算 int64。

### 5.2 Payment 狀態機

```
created ──▶ requires_action(3DS) ──▶ authorized ──▶ captured ──▶ (partially_)refunded
   │  │            │    │               │              │
   │  ▼            ▼    │               ▼              ▼
   │ failed      failed │             voided       disputed ──▶ chargeback_won / chargeback_lost
   ▼                    ▼               ▲
 expired ◀──────── expired              │ auth_expires_at 到期未 capture
                                        └─ (reason=authorization_expired)
```

狀態全集（與 `migrations/payment` 的 CHECK 一致）：`created, requires_action, authorized, captured, partially_refunded, refunded, voided, failed, expired, disputed, chargeback_won, chargeback_lost`。

- `expired`：`created` / `requires_action` 逾時未完成；`authorized` 超過授權有效期則轉 `voided`（reason=`authorization_expired`），不走 `expired`。
- 部分 capture（v1 僅允許單次 capture）：狀態仍為 `captured`，`amount_captured < amount_authorized`，餘額視同 void。
- `capture_method`: `automatic`（authorize+capture 一次完成）/ `manual`
- 每次轉移寫一筆 `payment_events`（append-only）並透過 outbox 發佈 `pg.payment.v1.PaymentEvent`。

### 5.3 Refund 狀態機
`pending ──▶ succeeded | failed`；受 `payment.captured_amount - refunded_amount` 約束。

### 5.4 Ledger（雙式記帳）
- 帳戶類型：`merchant_payable`、`psp_receivable`、`fee_revenue`、`refund_clearing`、`chargeback_reserve`
- 每筆 journal 的借貸總額必須相等（DB constraint + 應用層驗證）。
- 帳本**只能 INSERT**，錯帳用反向分錄沖銷。

## 6. 關鍵橫切機制

### 6.1 冪等（Idempotency）
- 所有寫入 REST API 必須帶 `Idempotency-Key`（UUID，商戶範圍內唯一，24h）。
- api-gateway：Valkey `SETNX` 鎖 + 儲存 `(request_hash, response)`；同 key 不同 payload → `409`。
- 服務層：`payment-service` 以 `(merchant_id, idempotency_key)` 唯一索引做最後防線。

### 6.2 Transactional Outbox
- 每個擁有 DB 的服務都有 `outbox` 表；業務資料與事件在同一交易寫入。
- `pkg/outbox` 提供 relay worker（polling + `FOR UPDATE SKIP LOCKED`）送到 Kafka；消費者以 `event_id` 去重（`processed_events` 表）。

### 6.3 Saga（付款流程）
`payment-service` 為 orchestrator：
1. 建立 payment（created）→ outbox `payment.created`
2. 選擇 provider（routing rules：幣別、卡種、商戶偏好、健康度、成本）
3. gRPC `Authorize` → 成功則 `authorized`（或 `requires_action`）；失敗且可重試 → failover 到下一個 provider
4. `Capture` → `captured` → outbox `payment.captured`
5. ledger-service 消費 `payment.captured` → 記帳；webhook-service 消費 → 通知商戶

### 6.4 安全
- 商戶驗證：`Authorization: Bearer pk_live_xxx` + `X-Timestamp` + `X-Signature: v1=<hex>`。簽章 = HMAC-SHA256(signing_secret, canonical)，canonical = `timestamp \n METHOD \n request_target(path[?query]) \n hex(sha256(body))`；±300 秒時間窗 + 簽章重放偵測（細節見 06 §3.3）。
- API Key 只存 Argon2id hash；顯示前綴 `pk_live_ab12…`。
- 對商戶 Webhook：`X-PG-Signature: t=<ts>,v1=<hmac>`，secret 可輪替（同時接受兩把）。
- PSP inbound webhook：由對應 adapter 驗簽（`ParseWebhook`）。
- 服務間 mTLS（生產）、最小權限 DB 帳號、Vault 動態憑證。
- 細節見 `06-security-compliance.md`。

### 6.5 可觀測性
- 每個請求一個 `trace_id`；`payment_id` 放進 span attribute 與 log field。
- 關鍵指標：`pg_payment_total{status,provider,currency}`、`pg_provider_latency_seconds`、`pg_webhook_delivery_total{result}`、`pg_outbox_lag_seconds`、`pg_ledger_imbalance_total`（必須恆為 0）。

## 7. Monorepo 目錄結構

```
PaymentGateway/
├── go.mod                         # github.com/tenghongzou/paymentgateway
├── Makefile
├── cmd/
│   ├── api-gateway/main.go
│   ├── merchant-service/main.go
│   ├── payment-service/main.go
│   ├── ledger-service/main.go
│   ├── webhook-service/main.go
│   ├── reconciliation-service/main.go
│   ├── provider-mock/main.go
│   └── provider-stripe/main.go
├── internal/                      # 每個服務一個子目錄，彼此不可 import
│   └── <service>/
│       ├── domain/                # 實體、值物件、狀態機、領域錯誤（無外部依賴）
│       ├── app/                   # use cases / application services、ports（interfaces）
│       └── adapter/
│           ├── grpc/              # gRPC server 實作
│           ├── http/              # 只有 api-gateway
│           ├── postgres/          # repository 實作
│           └── kafka/             # producer / consumer
├── pkg/                           # 跨服務共用、可被外部 import 的套件
│   ├── money/
│   ├── idempotency/
│   ├── outbox/
│   ├── eventbus/                  # Kafka 封裝
│   ├── pgdb/                      # pgx pool、migration helper、tx helper
│   ├── grpcx/                     # server/client 建構、interceptors（otel、logging、recovery）
│   ├── httpx/                     # middleware、error 回應格式
│   ├── otel/
│   ├── config/
│   └── sig/                       # HMAC 簽章 / 驗證
├── api/
│   ├── proto/pg/<service>/v1/*.proto
│   ├── gen/go/...                 # protoc 產物（commit 進 repo）
│   └── openapi/payment-gateway.yaml
├── migrations/<service>/NNNN_*.up.sql / .down.sql
├── deploy/
│   ├── docker/Dockerfile          # 多階段、以 build-arg 指定 service
│   ├── compose/docker-compose.yaml
│   ├── helm/paymentgateway/
│   └── otel/
├── .github/workflows/ci.yaml
└── docs/
```

### Import 規則
- `internal/<a>` **不得** import `internal/<b>`；共用邏輯下沉到 `pkg/`。
- `domain` 不得 import `app`/`adapter`；`app` 不得 import `adapter`（依賴反轉）。
- 服務間只透過 `api/gen/go`（gRPC client）或 Kafka 事件溝通。

## 8. 錯誤格式（REST）

```json
{ "error": { "type": "invalid_request_error", "code": "amount_too_small", "message": "...", "param": "amount", "request_id": "req_..." } }
```
`type` ∈ `invalid_request_error | authentication_error | idempotency_error | rate_limit_error | provider_error | api_error`。

## 9. 交付階段

| Phase | 內容 |
|---|---|
| 0（本次） | 架構、API、資料模型、安全、部署設計；可編譯的 monorepo 骨架；provider-mock 端到端跑通 |
| 1 | Stripe adapter、ledger 完整、webhook 重試、基本對帳 |
| 2 | risk-service、smart routing、Adyen/LINE Pay adapter、商戶後台 |
| 3 | 結算/撥款（payout）、多幣別、報表 |

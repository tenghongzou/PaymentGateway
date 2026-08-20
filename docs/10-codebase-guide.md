# PaymentGateway — 程式碼導覽（Codebase Guide）

> 對應 `docs/01-architecture.md` §7 目錄結構。本文件說明 Phase 0 骨架的實際程式組織、各層職責、如何擴充與測試，
> 以及常見陷阱。架構決策請看 01 與 ADR，不要在本文件改決策。

## 1. 目錄導覽

```
PaymentGateway/
├── cmd/<service>/main.go        # 8 個進入點，每個只有數十行：宣告 Config（內嵌 config.Base）→ app.Run(setup)
├── internal/
│   ├── payment/                 # payment-service（Phase 0 已完整實作三層）
│   │   ├── domain/              # Payment / Attempt / Refund 聚合、狀態機、ProviderErrorCategory、領域錯誤
│   │   ├── app/                 # use cases（CreatePayment / Capture / Void / Confirm / CreateRefund / 查詢）與 ports
│   │   └── adapter/
│   │       ├── grpc/            # pg.payment.v1.PaymentService 實作 + domain ↔ proto 轉換
│   │       ├── postgres/        # PaymentRepo / TxManager / OutboxStore（pgx；SQL 對齊 migrations/payment）
│   │       └── provider/        # ProviderAdapter gRPC client、Registry、Router、circuit breaker
│   ├── gateway/                 # api-gateway：Auth / Idempotency / RateLimit middleware、REST handlers、JSON 轉換
│   ├── providermock/            # provider-mock：token 決定情境的模擬 PSP（無 DB，無分層）
│   ├── merchant/ ledger/ webhook/ reconciliation/ providerstripe/   # 骨架：doc.go + adapter/grpc Unimplemented
├── pkg/                         # 跨服務共用（不得 import internal/）
│   ├── app/        服務啟動骨架：設定 → logger → OTel → auto migrate → HTTP(/healthz /readyz /metrics) → gRPC → 優雅關機；migrate 子命令
│   ├── apperr/     統一業務錯誤（type/code/param + HTTP 狀態表）
│   ├── config/     PG_* 環境變數（caarlos0/env），Base + Load[T]()
│   ├── eventbus/   franz-go Producer（實作 outbox.Publisher）/ Consumer（手動 commit、重試、DLQ）
│   ├── grpcx/      gRPC server/client 建構、interceptors、apperr ↔ gRPC status（ErrorDetail）
│   ├── httpx/      REST 錯誤格式、JSON、RequestID/Recover/Logging/Timeout middleware、gRPC → REST 錯誤
│   ├── idempotency/ Idempotency-Key 儲存（Redis / 記憶體）
│   ├── ids/        pay_/att_/re_/… 公開 ID（prefix + base32(UUIDv7)）
│   ├── logx/       slog JSON、trace_id/request_id 注入、MaskPAN/MaskSecret
│   ├── money/      Money 值物件（int64 最小單位、幣別小數位表、half-up bps）
│   ├── otel/       traces/metrics 初始化（OTLP + Prometheus reader）
│   ├── outbox/     Store.Insert / Relay（SKIP LOCKED）/ Inbox.MarkProcessed
│   ├── pgdb/       pgxpool、golang-migrate（embed.FS）、WithTx、錯誤判斷
│   └── sig/        商戶請求簽章（v1=，四行 canonical）與 webhook 簽章（t=,v1=）
├── migrations/                  # SQL + embed.go（所有服務的 migration 內嵌進每個二進位）
├── api/proto → api/gen/go       # buf generate 產物（commit 進 repo）
├── scripts/dev-pay.sh           # 用 PG_DEV_* 憑證打本機 gateway
└── test/e2e/                    # -tags e2e，對 localhost:8080 跑完整流程
```

## 2. 各層職責與依賴方向

| 層 | 可以 import | 不可 import | 內容 |
|---|---|---|---|
| `domain` | 標準庫、`pkg/money`、`pkg/ids`、`pkg/apperr` | `app`、`adapter`、pgx / grpc / kafka / redis | 純邏輯：狀態機（`CanTransition`）、聚合方法（`Authorize`/`Capture`/`Void`/`Fail`/`Expire`/`ReserveRefund`/`MarkRefunded`…）回傳要寫入的 `Event`；錯誤帶 01 §8 的 type/code |
| `app` | `domain`、`pkg/*`、`api/gen`（proto 型別） | `adapter` | use case 編排：交易邊界（`TxManager`）、兩階段寫入（鎖 → 預留 → 外部呼叫 → 套用）、failover 迴圈、事件 → outbox |
| `adapter` | 全部 | 其他服務的 `internal/<b>` | 實作 ports：Postgres repo、gRPC server、provider client/router |

`golangci-lint` 的 `depguard` 規則會擋住違規 import（`.golangci.yaml`）。
**注意**：`.golangci.yaml` 的服務隔離規則目前以 `internal/payment-service` 等名稱撰寫，實際目錄為 `internal/payment`、`internal/gateway`、`internal/providermock`；
分層規則（`internal/*/domain/**`）不受影響，但服務隔離規則需由 lint 維護者改成 `internal/*` glob 才會生效。

## 3. 請求在 payment-service 裡怎麼走（CreatePayment）

1. `adapter/grpc.Server.CreatePayment` 把 proto 轉成 `app.CreatePaymentCommand`（含 gateway 經 metadata `x-pg-request-hash` 傳入的 request hash）。
2. `app.Service.CreatePayment`：
   - 以 `(merchant_id, idempotency_key)` 查既有 → 同 hash 回放 / 不同 hash `idempotency_key_payload_mismatch`。
   - `domain.NewPayment` 驗證並產生 `payment.created`（version 1）。
   - `Router.Route` 產生有序候選（token_provider 鎖定 → preferred → 預設順序；幣別硬過濾；circuit open 排除）；無候選 → `no_route_available`，不落庫。
   - **tx1**：INSERT payments + 第一個 attempt + payment_events + outbox（唯一索引在此生效；衝突走回放）。
   - `runAuthorizeSaga`：對每個 attempt 呼叫 adapter `Authorize`（10s）→ `evaluateAuthorize` 正規化成 approved / requires_action / failed(category)。
     - approved → `Payment.Authorize`（+ automatic 時 `Capture`）→ **tx** 寫 payment / attempt / events / outbox。
     - requires_action → `RequireAction`，next_action 存在 attempt 的 `response_snapshot`。
     - failed → 依 `ProviderErrorCategory` 與 `Attempt.CanFailover()` 決定 failover（新 attempt、新 PSP 冪等鍵）或 `Fail`。
     - timeout（unknown）→ 先 `GetPaymentStatus` 收斂（1s/2s/4s）；仍不明則 payment 留在 `created`，不 failover。
3. 所有轉移都走 `Service.persist`：`UpdatePayment(WHERE version = $expected)` → `UpdateAttempt` → `AppendEvents(seq = version)` → `outbox.Insert(PaymentEvent protobuf)`。
4. 同進程的 `outbox.Relay` 把 outbox 送到 Kafka `payment.events`（key = `pay_…`）/ `refund.events`。

## 4. 如何新增一個 use case

以「CancelRefund」為例：

1. **domain**：在 `internal/payment/domain` 加聚合方法（例如 `Refund.Cancel(now) error` 與 `Payment.ReleaseRefund` 已有），需要新事件型別就加 `EventRefundCanceled` 常數；補 table-driven 測試。
2. **app**：在 `operations.go`（或新檔）加 `CancelRefundCommand` 與 `(*Service).CancelRefund`；遵守「tx1 驗證 → 外部呼叫（無鎖）→ tx2 套用（version 檢查）」；用 `persist` 寫入；在 `service_test.go` 以 fake ports 覆蓋成功 / 失敗 / 併發。
3. **events**：在 `app/events.go` 的 `buildOutboxMessage` 補 proto payload 對應（proto 需先在 `api/proto/pg/payment/v1/events.proto` 加欄位並 `make proto`）。
4. **adapter/grpc**：在 `server.go` 實作對應 rpc（proto 先加 rpc），錯誤一律 `grpcx.ErrorFromDomain(err)`。
5. **gateway**：`internal/gateway/handlers.go` 加 handler、`gateway.go` 加路由、`convert.go` 加 JSON 結構（欄位名以 OpenAPI 為準）；`gateway_test.go` 補 httptest。
6. 若需要新欄位：新增 `migrations/payment/000N_*.up/down.sql`（expand/contract，見 07 §4.3），更新 `adapter/postgres/repo.go` 的欄位清單與 scan。

## 5. 如何新增一個 provider adapter

1. 建 `internal/provider<name>/`（參考 `internal/providerstripe/doc.go` 的結構）：`adapter/grpc/server.go` 內嵌 `providerv1.UnimplementedProviderAdapterServer`，逐一實作 `Authorize / Capture / Void / Refund / GetPaymentStatus / ParseWebhook / HealthCheck`。
2. 錯誤分類：**業務拒絕不得回 gRPC error**，用 `ProviderResult{success=false, error_category, provider_error_code}`；只有 adapter 自身故障 / PSP 逾時 / 不可達才回 gRPC `INTERNAL` / `DEADLINE_EXCEEDED` / `UNAVAILABLE`。payment-service 的 `app/provider_result.go` 依此映射到 `domain.ProviderErrorCategory`（docs/02 §11）。
3. `provider_error_code` 請填 docs/02 §11.1 的正規化 decline code（`insufficient_funds`、`try_again_later`…），failover 白名單與 `retryable` 都靠它。
4. `HealthCheck` 回 `capabilities.currencies`（Router 用來做幣別硬過濾）；未就緒時回 `NOT_SERVING`（會被路由排除）。
5. 加 `cmd/provider-<name>/main.go`（複製 provider-stripe），在 compose / Helm 加 `PG_PROVIDER_ADDRS` 的 `<name>=host:port`，gateway 的 `/psp/<name>/webhook` 會自動對應。
6. 契約測試：用 `internal/providermock/service_test.go` 的 table-driven 形式覆蓋各分類。

## 6. 測試怎麼跑

| 指令 | 內容 | 需要 |
|---|---|---|
| `go test ./...` | 單元測試（domain 狀態機全矩陣、app fake ports、gateway httptest、pkg/*） | 無（< 2 分鐘，無 docker） |
| `go test -tags integration ./...` | testcontainers：`internal/payment/adapter/postgres`（套用 migrations/payment）、`pkg/idempotency`（Redis）、`pkg/eventbus`（Kafka） | Docker |
| `make e2e` 或 `go test -tags e2e ./test/e2e/` | 對 `http://localhost:8080` 跑建立 / 查詢 / 退款 / decline / failover / 3DS / void | compose 全套或三個服務本機啟動 |
| `./scripts/dev-pay.sh` | 用 `PG_DEV_*` 憑證建立一筆 `tok_ok` 付款並查詢 | gateway + payment-service + provider-mock |

本機不用 compose 跑三個服務（需先 `make compose-infra`）：

```bash
PG_SERVICE_NAME=provider-mock PG_GRPC_ADDR=:9101 PG_HTTP_ADDR=:18101 go run ./cmd/provider-mock
PG_SERVICE_NAME=payment-service PG_GRPC_ADDR=:9002 PG_HTTP_ADDR=:18002 PG_AUTO_MIGRATE=true \
  PG_DATABASE_URL='postgres://payment_owner:payment_owner@localhost:5432/pg_payment?sslmode=disable' \
  PG_KAFKA_BROKERS=localhost:29092 PG_PROVIDER_ADDRS=mock=localhost:9101 go run ./cmd/payment-service
PG_SERVICE_NAME=api-gateway PG_HTTP_ADDR=:8080 PG_REDIS_ADDR=localhost:6379 PG_PAYMENT_SERVICE_ADDR=localhost:9002 \
  PG_PROVIDER_ADDRS=mock=localhost:9101 PG_DEV_API_KEY=pk_test_dev_0000000000000000 \
  PG_DEV_SIGNING_SECRET=sk_test_dev_secret_change_me PG_DEV_MERCHANT_ID=0190a1b2-c3d4-7e5f-8a9b-000000000001 go run ./cmd/api-gateway
```

沒有 Kafka 時 payment-service 可加 `PG_OUTBOX_DISABLED=true`（事件留在 outbox 表）。

## 7. 常見陷阱

- **Money**：金額一律 `pkg/money.Money`，`Add/Sub/MulBps` 會回錯（幣別不符、溢位、負數），不要直接算 int64；`forbidigo` 在 money/domain/app 禁用 float。
- **version / seq**：每個領域事件讓 `Payment.Version` +1，`payment_events.seq` = 事件發生後的 version；`UpdatePayment(expectedVersion)` 必須傳「讀取時」的版本，0 列影響 → `pgdb.ErrConcurrentModification`（gateway 回 409 `concurrent_modification`）。同一交易寫多個事件（T3 authorized + captured）時 `persist` 的 expected 是第一個事件前的版本。
- **不要在持有 DB 鎖時呼叫 PSP**：use case 都是「tx1 驗證 → 無鎖呼叫 → tx2 套用」；tx2 用 `context.WithoutCancel`，避免 client 取消導致 PSP 已扣款但本地未記錄。
- **context deadline**：adapter 呼叫用 `PG_PROVIDER_TIMEOUT`（10s），整個 authorize saga 25s，剩餘 < 3s 不再 failover；`tok_timeout` 會把 deadline 用盡，測試時請給足時間。
- **timeout ≠ failover**：`provider_timeout`/`unknown` 必須先 `GetPaymentStatus` 收斂；只有 `not_found` 才視為 unavailable 可切換。
- **ID 前綴**：refund 是 `re_`、dispute 是 `dp_`、webhook endpoint 是 `we_`（SQL CHECK 約束），與 02 §0.2 表格的 `ref_/dsp_/whe_` 不同，以 SQL 為準。
- **livemode**：`payments`/`refunds` 表目前沒有 `livemode` 欄位，payment-service 不落庫；gateway 以 API key mode 回填回應的 `livemode`。
- **簽章**：`X-Signature: v1=<hex>`；canonical 四行（ts / METHOD / request target 含 query / sha256(body)）。GET 的 body hash 是 `sha256("")`。同一簽章 300s 內重送會被 `signature_replayed` 拒絕，重試必須重新簽。
- **Idempotency-Key**：寫入端點必帶；request hash 含 method + path + canonical JSON，同 key 用在不同端點會 409。5xx 不快取。
- **config**：所有服務設定 struct 必須內嵌 `config.Base`，否則 `pkg/app` 會以 exit code 2 結束。
- **zsh**：shell 腳本裡不要用 `path` 當變數名（zsh 會連動 `PATH`）。

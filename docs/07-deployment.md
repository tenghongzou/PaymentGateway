# PaymentGateway — 部署與維運（Deployment & Operations）

> 對應 `docs/01-architecture.md` §3 技術棧、§4 服務與 port、§7 目錄結構。本文件由 DevOps 維護；
> 改變 port / 服務清單 / 環境變數命名請先改 01 並新增 ADR。

## 0. 檔案索引

| 路徑 | 用途 |
|---|---|
| `Makefile` | 所有開發/建置/測試/部署入口（`make help`） |
| `buf.yaml` / `buf.gen.yaml` / `scripts/protoc-gen.sh` | Protobuf lint / breaking / codegen（無 buf 時 fallback） |
| `deploy/docker/Dockerfile` + `.dockerignore` | 多階段、`--build-arg SERVICE=<svc>`，distroless nonroot |
| `deploy/compose/docker-compose.yaml` + `.env.example` | 本機全套（infra + 8 個服務） |
| `deploy/compose/postgres/init.sql` | **DBA 提供**：建立 5 個 database 與角色（首次啟動時執行） |
| `deploy/otel/collector.yaml` / `prometheus.yaml` / `alerts.yaml` / `grafana/` | 可觀測性設定與告警規則 |
| `deploy/helm/paymentgateway/` | 生產用 Helm chart（`values-staging.yaml`、`values-production.yaml`） |
| `deploy/k8s/migrations-job.yaml` | 獨立 migration Job（首次安裝 / 事故回滾） |
| `.github/workflows/ci.yaml` / `release.yaml` | CI（lint/proto/test/integration/build/security/helm）與發版 |
| `.golangci.yaml` | Lint 規則（含 import 邊界與 money 禁用 float） |

## 1. 本機開發流程

### 1.1 前置需求

- Go 1.26、Docker Desktop（含 `docker compose` v2）、`protoc`（僅在沒有 buf 時需要）、`curl`
- 建議：`grpcurl`、`jq`、`psql`

### 1.2 三行啟動

```bash
make tools          # 安裝 protoc-gen-go / protoc-gen-go-grpc / golangci-lint / migrate / buf 到 ./bin
make proto          # buf generate → api/gen/go（沒有 buf 會自動改用 scripts/protoc-gen.sh）
make compose-up     # 建 image + 啟動 postgres/redis/kafka/otel/jaeger/prometheus/grafana + 8 個服務
```

其他常用：

```bash
COMPOSE_PROFILES=tools make compose-up   # 多開 kafka-ui（:8090）與 vault dev（:8200, token=root）
make compose-logs S=payment-service      # 追 log
make compose-ps                          # 狀態
make compose-infra                       # 只起 infra，服務用 IDE 跑（見 1.6）
make compose-down                        # 關閉並刪 volume
make e2e                                 # 起 compose → 等 /healthz → go test -tags e2e ./test/e2e/...
```

`deploy/compose/.env.example` 複製為 `deploy/compose/.env` 可覆寫 port、log level、Stripe 測試金鑰等。

### 1.3 本機 port 對照

| 服務 | 容器內 | Host |
|---|---|---|
| api-gateway REST | 8080 | `localhost:8080` |
| merchant-service gRPC / health | 9001 / 8081 | `localhost:9001` / `localhost:18001` |
| payment-service gRPC / health | 9002 / 8081 | `localhost:9002` / `localhost:18002` |
| ledger-service gRPC / health | 9003 / 8081 | `localhost:9003` / `localhost:18003` |
| webhook-service gRPC / health | 9004 / 8081 | `localhost:9004` / `localhost:18004` |
| reconciliation-service gRPC / health | 9005 / 8081 | `localhost:9005` / `localhost:18005` |
| provider-mock gRPC / health | 9101 / 8081 | `localhost:9101` / `localhost:18101` |
| provider-stripe gRPC / health | 9102 / 8081 | `localhost:9102` / `localhost:18102` |
| PostgreSQL | 5432 | `localhost:5432`（superuser `postgres/postgres`；服務角色 `<svc>_owner` / `<svc>_app`，密碼同角色名，見 `deploy/compose/postgres/init.sql`） |
| Redis | 6379 | `localhost:6379` |
| Kafka（host listener） | 29092 | `localhost:29092`（容器內用 `kafka:9092`） |
| OTel Collector OTLP gRPC / HTTP | 4317 / 4318 | 同 |
| Jaeger UI | 16686 | http://localhost:16686 |
| Prometheus | 9090 | http://localhost:9090（/alerts 可看規則狀態） |
| Grafana | 3000 | http://localhost:3000（匿名 Admin，Dashboard「PaymentGateway / Overview」） |
| Kafka UI（profile tools） | 8080 | http://localhost:8090 |
| Vault dev（profile tools） | 8200 | http://localhost:8200（token `root`） |

### 1.4 用 curl 打 gateway

```bash
# 健康檢查
curl -s localhost:8080/healthz

# 建立付款（簽章規則見 06 §3.3：canonical = timestamp\nMETHOD\nrequest_target\nsha256(body)，X-Signature: v1=<hex>，±300 秒；可直接用 scripts/dev-pay.sh）
API_KEY=pk_test_xxx; SECRET=sk_secret_xxx
BODY='{"amount":1000,"currency":"TWD","capture_method":"automatic","provider":"mock"}'
TS=$(date +%s)
SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')
curl -s -X POST localhost:8080/v1/payments \
  -H "Authorization: Bearer $API_KEY" \
  -H "X-Timestamp: $TS" -H "X-Signature: $SIG" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H 'Content-Type: application/json' -d "$BODY" | jq .
```

測試商戶與 API key 由 merchant-service 的 seed（`migrations/merchant/`）或 `grpcurl` 建立，見 `03-api.md`。

### 1.5 用 grpcurl 打服務

服務在 `PG_ENV=dev` 時啟用 gRPC reflection（生產關閉）：

```bash
grpcurl -plaintext localhost:9002 list
grpcurl -plaintext localhost:9002 describe pg.payment.v1.PaymentService
grpcurl -plaintext -d '{"payment_id":"pay_123"}' localhost:9002 pg.payment.v1.PaymentService/GetPayment

# 沒有 reflection 時直接帶 proto
grpcurl -plaintext -import-path api/proto -proto pg/provider/v1/provider.proto \
  -d '{}' localhost:9101 pg.provider.v1.ProviderAdapter/HealthCheck

# 標準 gRPC health（k8s / probes 也可用）
grpcurl -plaintext localhost:9002 grpc.health.v1.Health/Check
```

### 1.6 在 IDE 跑單一服務

```bash
make compose-infra
export PG_SERVICE_NAME=payment-service PG_ENV=dev PG_LOG_LEVEL=debug \
       PG_GRPC_ADDR=:9002 PG_HTTP_ADDR=:8081 \
       PG_DATABASE_URL='postgres://payment_owner:payment_owner@localhost:5432/pg_payment?sslmode=disable' \
       PG_REDIS_ADDR=localhost:6379 PG_KAFKA_BROKERS=localhost:29092 \
       PG_OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 PG_OTEL_EXPORTER_OTLP_INSECURE=true \
       PG_MERCHANT_SERVICE_ADDR=localhost:9001 PG_PROVIDER_ADDRS=mock=localhost:9101 \
       PG_AUTO_MIGRATE=true PG_MIGRATIONS_DIR=$PWD/migrations
go run ./cmd/payment-service
```

### 1.7 Migrations

```bash
make migrate-create SVC=payment NAME=add_refund_reason   # migrations/payment/000N_add_refund_reason.{up,down}.sql
make migrate-up SVC=payment                              # 用 ./bin/migrate 對 DATABASE_URL 執行
make migrate-down SVC=payment MIGRATE_STEPS=1
make migrate-up-all                                      # 五個 DB 依序
DATABASE_URL=postgres://... make migrate-up SVC=ledger    # 覆寫連線
```

目錄、DB 與角色對照（**全隊固定**；角色由 DBA 的 `deploy/compose/postgres/init.sql` 建立，`*_owner` 有 DDL、`*_app` 只有 DML）：

| 服務 | `migrations/<dir>` | Database | DDL 角色（migration / dev） | DML 角色（staging / prod 服務） |
|---|---|---|---|---|
| merchant-service | `merchant` | `pg_merchant` | `merchant_owner` | `merchant_app` |
| payment-service | `payment` | `pg_payment` | `payment_owner` | `payment_app` |
| ledger-service | `ledger` | `pg_ledger` | `ledger_owner` | `ledger_app` |
| webhook-service | `webhook` | `pg_webhook` | `webhook_owner` | `webhook_app` |
| reconciliation-service | `reconciliation` | `pg_recon` | `recon_owner` | `recon_app` |

compose 的 `PG_DATABASE_URL` 為 `postgres://<short>_owner:<short>_owner@postgres:5432/pg_<short>?sslmode=disable`（因為 `PG_AUTO_MIGRATE=true` 需要 DDL）。
Kubernetes：Vault 提供 `PG_DATABASE_URL`（`*_app`）給服務、`PG_MIGRATE_DATABASE_URL`（`*_owner`）給 migration Job。

### 1.8 服務二進位需遵守的運維契約（給 golang-pro）

部署設定依賴以下行為，請在 `pkg/` 共用實作：

1. **HTTP 端點**（`PG_HTTP_ADDR`）：`GET /healthz`（process 活著即 200）、`GET /readyz`（DB/Redis/Kafka 連得上且 migration 完成才 200）、`GET /metrics`（可選；主要走 OTLP）。api-gateway 的這三個路徑與業務 API 同 port 8080，並排除在認證與限流之外。
2. **gRPC**：註冊 `grpc.health.v1.Health`；`PG_ENV=dev|staging` 時啟用 reflection。
3. **優雅關機**：收到 SIGTERM 後先讓 `/readyz` 回 503，等待 in-flight 請求（上限 `terminationGracePeriodSeconds` 30s），再關 Kafka consumer、outbox relay、DB pool。容器是 distroless，沒有 shell，不可依賴 preStop 指令。
4. **`PG_AUTO_MIGRATE=true`** 時在啟動流程最前面對 `$PG_MIGRATIONS_DIR/<short-name>` 執行 golang-migrate up（多副本同時啟動要用 advisory lock；golang-migrate 的 postgres driver 內建）。
5. **`migrate` 子命令**：`/app migrate up|down [N]|version`，讀同一組 `PG_DATABASE_URL` / `PG_MIGRATIONS_DIR`，結束碼非 0 代表失敗。Helm pre-upgrade hook 與 `deploy/k8s/migrations-job.yaml` 都靠它。
6. **版本資訊**：`package main` 宣告 `var version, commit, buildDate string`，由 `-ldflags -X main.version=...` 注入，啟動 log 與 `/healthz` 回應帶上。
7. **唯讀根檔案系統**：只能寫 `/tmp`（emptyDir / tmpfs）。
8. 設定用 `caarlos0/env`，未知的 `PG_*` 變數必須忽略（compose 對所有服務注入同一組共用變數）。

## 2. 環境變數總表（`PG_` 前綴）

| 變數 | 說明 | 預設 | 必填 | 誰用 |
|---|---|---|---|---|
| `PG_SERVICE_NAME` | 服務名稱，進 log / OTel `service.name` | image 內建 `SERVICE` | 是 | 全部 |
| `PG_ENV` | `dev` / `staging` / `production`；影響 reflection、debug 端點、log 格式 | `dev` | 是 | 全部 |
| `PG_LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` | 否 | 全部 |
| `PG_HTTP_ADDR` | HTTP 監聽位址；gateway `:8080`，其他 `:8081`（health/metrics） | `:8081` | 是 | 全部 |
| `PG_GRPC_ADDR` | gRPC 監聽位址（`:9001`…`:9102`） | 無 | gRPC 服務必填 | 非 gateway |
| `PG_DATABASE_URL` | pgx 連線字串；生產 `sslmode=require`、`*_app` 角色，由 Vault 注入 | 無 | 擁有 DB 的服務必填 | merchant/payment/ledger/webhook/recon |
| `PG_MIGRATE_DATABASE_URL` | migration 用的 `*_owner`（DDL）連線；只給 migration Job，缺省時 Job 退回 `PG_DATABASE_URL` | 無 | 否 | migration Job |
| `PG_AUTO_MIGRATE` | 啟動時自動執行 migration | `false` | 否 | 擁有 DB 的服務 |
| `PG_MIGRATIONS_DIR` | migration 根目錄（服務自行加 `<short-name>`） | `/migrations` | 否 | 擁有 DB 的服務 |
| `PG_REDIS_ADDR` | `host:port` | 無 | gateway 必填 | gateway（冪等/限流）、其餘可選快取 |
| `PG_REDIS_PASSWORD` | Redis 密碼（Vault） | 空 | 生產必填 | 同上 |
| `PG_KAFKA_BROKERS` | 逗號分隔 broker | 無 | 是 | 全部（provider 可不用） |
| `PG_KAFKA_CONSUMER_GROUP` | consumer group 名稱 | `PG_SERVICE_NAME` | 否 | ledger/webhook/recon |
| `PG_KAFKA_SASL_USERNAME` / `PG_KAFKA_SASL_PASSWORD` / `PG_KAFKA_TLS` | MSK SASL/SCRAM 或 IAM；生產走 TLS | 空 / `false` | 生產必填 | 全部 |
| `PG_OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP gRPC collector 位址 | 無（空 = 不輸出） | 否 | 全部 |
| `PG_OTEL_EXPORTER_OTLP_INSECURE` | 不使用 TLS 連 collector | `false` | 否 | 全部 |
| `PG_MERCHANT_SERVICE_ADDR` | merchant gRPC 位址 | 無 | gateway/payment/webhook 必填 | |
| `PG_PAYMENT_SERVICE_ADDR` | payment gRPC 位址 | 無 | gateway/recon 必填 | |
| `PG_LEDGER_SERVICE_ADDR` | ledger gRPC 位址 | 無 | gateway/recon 必填 | |
| `PG_WEBHOOK_SERVICE_ADDR` | webhook gRPC 位址 | 無 | gateway 必填 | |
| `PG_RECONCILIATION_SERVICE_ADDR` | reconciliation gRPC 位址 | 無 | gateway 必填 | |
| `PG_PROVIDER_ADDRS` | `key=host:port,...`，如 `mock=provider-mock:9101,stripe=provider-stripe:9102` | 無 | payment 必填 | payment |
| `PG_PROVIDER_TIMEOUT` | 單次 provider 呼叫逾時 | `10s` | 否 | payment |
| `PG_PROVIDER_FAILOVER_MAX_ATTEMPTS` | failover 最多嘗試幾個 provider | `2` | 否 | payment |
| `PG_RATE_LIMIT_RPS` | 每商戶預設限流 | `100` | 否 | gateway |
| `PG_IDEMPOTENCY_TTL` | Idempotency-Key 保存時間 | `24h` | 否 | gateway |
| `PG_WEBHOOK_MAX_ATTEMPTS` / `PG_WEBHOOK_TIMEOUT` | 重試次數（依 06 §4.4 時程表）/ 單次逾時 | `10` / `10s` | 否 | webhook |
| `PG_STRIPE_API_KEY` / `PG_STRIPE_WEBHOOK_SECRET` / `PG_STRIPE_API_BASE_URL` | Stripe 憑證（Vault）、base URL（測試可指向 stripe-mock） | 無 / 無 / `https://api.stripe.com` | provider-stripe 必填 | provider-stripe |
| `PG_MOCK_DEFAULT_LATENCY` / `PG_MOCK_FAILURE_RATE` | mock PSP 模擬延遲與失敗率 | `50ms` / `0` | 否 | provider-mock |
| `PG_GRPC_TLS_CERT` / `PG_GRPC_TLS_KEY` / `PG_GRPC_TLS_CA` | 服務間 mTLS（生產；若用 service mesh 可省略） | 空 | 生產（無 mesh 時）必填 | 全部 |

另外 Kubernetes 會注入 `POD_NAME`、`POD_NAMESPACE`、`NODE_NAME`、`OTEL_SERVICE_NAME`、`OTEL_RESOURCE_ATTRIBUTES`、`GOMEMLIMIT`（= memory limit）。

祕密（`PG_DATABASE_URL`、`PG_REDIS_PASSWORD`、`PG_KAFKA_SASL_*`、`PG_STRIPE_*`、webhook signing secret 的 KMS 設定）在 Kubernetes 一律由 ExternalSecret 從 Vault KV-v2 拉取，路徑 `paymentgateway/<env>/common` 與 `paymentgateway/<env>/<service-name>`，**Vault 內的 key 就是環境變數名稱**。

## 3. 環境分層、分支與發版

| 環境 | 用途 | 部署方式 | 資料 |
|---|---|---|---|
| dev（本機） | 開發、`make e2e` | docker compose | 可隨時重建；provider-mock |
| staging | PR 合併後自動部署、整合測試、PSP 測試模式 | Helm + `values-staging.yaml`，image tag = `sha-<short>`；`PG_AUTO_MIGRATE=true` | 與 prod 同構（較小規格），每週從 prod 匿名化快照還原 |
| production | 正式 | Helm + `values-production.yaml`，image tag = `vX.Y.Z`；migration 走 pre-upgrade Job | 多 AZ、PITR |

分支策略（trunk-based）：

1. 所有變更走短命 feature branch → PR → `main`；PR 必須通過 CI 全部 job（lint、proto breaking、test、integration、build+trivy、security、helm）。
2. 合併到 `main` 後 CI 推 `sha-<short>` 與 `main` tag 的 image → Argo CD / `helm upgrade` 自動部署 staging。
3. 發版：在 `main` 上打 semver tag `vX.Y.Z`（pre-release 用 `vX.Y.Z-rc.N`）→ `release.yaml` 建 multi-arch image（cosign 簽章 + SBOM + provenance）、推 Helm chart 到 `oci://ghcr.io/<owner>/paymentgateway/charts`、建 GitHub Release（自動 release notes + image digest 表）。
4. 生產部署永遠用 **tag**，不用 `latest`；回滾 = `helm rollback` 或改回前一個 tag（DB 見 §4.3）。
5. Proto 相容性：`buf breaking` 以 `main` 為基準；破壞性變更必須開新版本 package（`v2`），不可修改 `v1`。

## 4. 部署策略

### 4.1 Rolling update + readiness gate（預設）

- `maxSurge: 25%`、`maxUnavailable: 0`；`readinessProbe /readyz` 未通過的 pod 不會收流量；`startupProbe` 最長 2 分鐘容忍 migration / 暖機。
- PDB：gateway / payment 至少 2（prod 3）個可用；其他 `minAvailable: 1`。
- `topologySpreadConstraints`：跨 zone `DoNotSchedule`（prod）、跨 node `ScheduleAnyway`。
- 停機順序：SIGTERM → readiness 503 → 等 in-flight（≤ 30s）→ 關 consumer → 退出。Kafka consumer 在關閉前 commit offset，避免重播風暴；消費者本來就以 `event_id` 去重。

### 4.2 Canary（建議 Argo Rollouts）

對 `api-gateway` 與 `payment-service` 採 canary：

```yaml
strategy:
  canary:
    canaryService: paymentgateway-api-gateway-canary
    stableService: paymentgateway-api-gateway
    trafficRouting: { nginx: { stableIngress: paymentgateway-api-gateway } }
    steps:
      - setWeight: 5
      - pause: { duration: 10m }
      - analysis: { templates: [{ templateName: pg-success-rate }] }   # 5xx < 1%、p99 < 300ms、pg_payment_total{status=failed} 比例未上升
      - setWeight: 25
      - pause: { duration: 15m }
      - setWeight: 50
      - pause: { duration: 15m }
```

AnalysisTemplate 查 Prometheus：`sum(rate(http_server_request_duration_seconds_count{status=~"5.."}[5m])) / sum(rate(...))`、provider error rate、ledger imbalance（任何 > 0 立即 abort）。把 Deployment 換成 Rollout 只需在 chart 加 `kind` 開關，Phase 1 再導入。

### 4.3 資料庫 migration：expand / contract

原則：**任何 migration 都必須與「前一個」和「下一個」版本的程式同時相容**，因為 rolling 期間新舊 pod 並存，且回滾程式不回滾 schema。

| 步驟 | 做什麼 | 範例 |
|---|---|---|
| Expand（release N） | 只加：新表、nullable 欄位、新索引（`CREATE INDEX CONCURRENTLY`，且放在獨立 migration、不在交易中）、新 enum 值 | 加 `refunds.reason TEXT NULL` |
| Migrate（release N / N+1） | 程式雙寫；背景 backfill 分批（每批 ≤ 5k 列、`pg_sleep`）；不可長時間鎖表 | backfill `reason` |
| Contract（release N+2） | 移除舊欄位 / 舊表 / `NOT NULL` 約束（用 `NOT VALID` + `VALIDATE CONSTRAINT` 兩步） | drop `refunds.legacy_reason` |

禁止：`ALTER TABLE ... TYPE` 改型、重新命名欄位、在大表上直接加 `NOT NULL DEFAULT`（PG 11+ 常數 default 安全，但仍避免）、非 `CONCURRENTLY` 建索引、`down.sql` 刪資料（down 只做結構回退）。

發版順序（每個服務獨立）：

1. `helm upgrade` 觸發 **pre-upgrade hook Job**（每個擁有 DB 的服務一個，`/app migrate up`），Job 失敗 → upgrade 中止，舊版本繼續服務。
2. hook 成功 → Deployment rolling。
3. 回滾：`helm rollback` 只回滾程式；schema 因 expand 相容所以不用動。只有 contract 出錯才手動 `/app migrate down 1`（用 `deploy/k8s/migrations-job.yaml`）。
4. 首次安裝：hook 只設 `pre-upgrade`（因為 ExternalSecret 產生的 Secret 在第一次安裝前不存在），改用 `--set global.autoMigrate=true` 安裝一次，或先 apply `deploy/k8s/migrations-job.yaml`。
5. 多服務相依的變更（例如 payment 事件新增欄位、ledger 消費）：先發 consumer（能讀新舊格式），再發 producer；Protobuf 只加欄位不改 tag。

## 5. 擴展策略

| 服務 | 主要擴展依據 | HPA / 建議 | 備註 |
|---|---|---|---|
| api-gateway | CPU 60%、RPS（`http_server_request_duration_seconds_count`） | 3 → 30/40 | 無狀態；Redis 是共享瓶頸，先看 Redis CPU |
| payment-service | CPU 60%、gRPC in-flight | 3 → 30/40 | 對 PSP 是 I/O bound，可用較多副本；失敗 failover 會放大 PSP 流量 |
| merchant-service | CPU 70% | 2 → 10 | 讀多寫少；API key 驗證結果可由 gateway 快取 60s |
| ledger-service | **Kafka consumer lag**（`kafka_consumergroup_lag{consumergroup="ledger-service"}`） | 2 → 12（= `payment.events` 分區數） | 副本數 ≤ 分區數；prod values 示範 External metric（prometheus-adapter 或 KEDA `ScaledObject`） |
| webhook-service | consumer lag + `pg_webhook_pending_deliveries` | 2 → 16 | dispatcher 是「有狀態 worker」：用 `FOR UPDATE SKIP LOCKED` 分批領取，多副本天然分片；外送 goroutine 數由 env 控制 |
| reconciliation-service | 無（批次） | 1，HPA 關閉 | 匯入工作用 DB advisory lock 或 Kafka 單分區保證單一執行者；Phase 1 可改 CronJob |
| provider-* | CPU、`pg_provider_latency_seconds` | 2 → 12 | 對外連線數注意 PSP 的 rate limit |

有狀態 worker（outbox relay、webhook dispatcher）：

- **Outbox relay**（每個擁有 DB 的服務內建）：每個副本都跑 relay，以 `SELECT ... FOR UPDATE SKIP LOCKED LIMIT 100` 取批次，天然分片、無需 leader。序列性需求（同一 aggregate 事件有序）靠 Kafka key = `payment_id` 與「同一 aggregate 的事件在同一批、同一 relay」保證；若要更嚴格，改成以 `hashtext(aggregate_id) % N = shard` 分片，N 由 `PG_OUTBOX_SHARDS` / pod ordinal 決定（StatefulSet）。
- **Leader election**（只給需要單一執行者的排程：對帳匯入、webhook 死信清理、過期冪等 key 清理）：用 PostgreSQL `pg_try_advisory_lock(key)`（最簡單、無額外依賴）或 `k8s.io/client-go/tools/leaderelection`（Lease）。
- Kafka 分區數建議：`payment.events` 12、`refund.events` 6、`ledger.events` 6、`merchant.events` 3、`reconciliation.events` 3；分區只增不減。

容量目標：500 TPS 初期 → 5k TPS。以 payment-service 每副本 ~150 TPS（含 PSP 等待）估算，5k TPS 需 ~35 副本 + PostgreSQL 寫入 ~10k rows/s（payment + events + outbox），屆時需 Aurora / 分區表（`payment_events` 按月分區）與 PgBouncer。

## 6. 高可用

- **Kubernetes**：3 AZ node group；每個服務跨 zone 分散；PDB 防止節點維護同時驅逐；`priorityClassName` 可給 gateway/payment 較高優先權。
- **PostgreSQL**：託管優先（RDS/Aurora PostgreSQL Multi-AZ，或 Cloud SQL HA）；自管則 Patroni + etcd + PgBouncer（transaction pooling；注意 `SKIP LOCKED` 與 advisory lock 需 session pooling 或在同一交易內）。每個服務獨立 database（可先同一 instance，之後拆 instance）。連線數：每服務 pool ≤ 20 × 副本數，總量受 `max_connections` 限制，超過即引入 PgBouncer。
- **Kafka**：3 broker、`replication.factor=3`、`min.insync.replicas=2`、producer `acks=all`、`enable.idempotence=true`；託管 MSK / Confluent。
- **Redis**：ElastiCache cluster-mode 或 Redis Sentinel（1 主 2 從）；冪等鎖資料若遺失，後果是「同 key 重送可能重複建單」——所以 payment-service 的 `(merchant_id, idempotency_key)` 唯一索引是最後防線；Redis 完全不可用時 gateway 對寫入 API 回 503（fail-closed）。
- **PSP**：routing 健康度（`pg_provider_healthy`、錯誤率）自動 failover；單一 PSP 故障不影響整體（01 §2）。
- **Ingress**：ingress-nginx ≥ 3 副本跨 AZ，前面 NLB；TLS 由 cert-manager 管理。

## 7. 災難復原

| 項目 | 目標 |
|---|---|
| RPO（資料遺失） | PostgreSQL ≤ 5 分鐘（PITR，WAL 連續歸檔）；Kafka 7 天保留可重播 |
| RTO（恢復時間） | 單 AZ 故障：自動，< 5 分鐘；區域故障：< 4 小時（IaC 重建 + 快照跨區複製） |

做法：

1. PostgreSQL：自動每日快照 + PITR；快照跨區複製；**每月自動還原演練**（CI job：還原到臨時 instance → 跑 `SELECT count(*)`、ledger 借貸平衡檢查 `SUM(debit) = SUM(credit)` → 銷毀）。
2. Kafka：事件可從 outbox 表重發（outbox 保留 7 天再清理），消費者去重保證重播安全。
3. Redis：不備份（冪等 key 24h、限流計數可丟）。
4. Vault：Raft 快照每日；unseal key 分持。
5. 設定與 IaC：Helm values、Terraform 全在 git；`helm get values` 定期匯出以偵測 drift。
6. 演練：每季一次 game day（kill 一個 AZ 的 node、模擬 PSP 全掛、模擬 Kafka 不可用 → 驗證 outbox 累積後自動追上）。

## 8. 監控與告警

### 8.1 指標來源

服務 → OTLP → Collector → Prometheus（`deploy/otel/collector.yaml`）。關鍵自訂指標（01 §6.5）：`pg_payment_total{status,provider,currency}`、`pg_provider_latency_seconds`、`pg_provider_request_total{provider,result}`、`pg_provider_healthy`、`pg_webhook_delivery_total{result}`、`pg_webhook_pending_deliveries`、`pg_outbox_lag_seconds`、`pg_outbox_pending`、`pg_outbox_published_total`、`pg_ledger_imbalance_total`、`pg_idempotency_errors_total`、`pg_rate_limit_rejected_total{merchant_id}`、`pg_reconciliation_mismatch_total{provider}`、`pg_db_pool_*`。HTTP/gRPC 延遲使用 OTel semconv 的 `http_server_request_duration_seconds`、`rpc_server_duration_milliseconds`。Kafka lag 由 kafka-exporter / MSK CloudWatch 匯入。

### 8.2 告警規則（`deploy/otel/alerts.yaml`）

| 告警 | 條件 | 嚴重度 | Runbook |
|---|---|---|---|
| `PGOutboxLagHigh` | `pg_outbox_lag_seconds > 30` 持續 5m | page | `runbooks/outbox-lag.md` |
| `PGOutboxRelayStalled` | 有 pending 但 10m 無發佈 | page | 同上 |
| `PGWebhookDeadLetterIncreasing` | 15m 內有死信 | ticket | `runbooks/webhook-dead-letter.md` |
| `PGWebhookFailureRateHigh` | 失敗比例 > 25% 持續 10m | page | 同上 |
| `PGWebhookBacklogHigh` | pending > 10k | ticket | 同上 |
| `PGProviderErrorRateHigh` | 單一 provider 錯誤率 > 10% 持續 5m | page | `runbooks/provider-degraded.md` |
| `PGProviderLatencyP99High` | p99 > 5s 持續 10m | ticket | 同上 |
| `PGProviderUnhealthy` | `pg_provider_healthy == 0` 2m | page | 同上 |
| `PGPaymentFailureRateHigh` | 全體失敗率 > 20% | page | 同上 |
| `PGLedgerImbalanceDetected` | `pg_ledger_imbalance_total` 任何增加 | **page（最高）** | `runbooks/ledger-imbalance.md` |
| `PGLedgerConsumerLagHigh` | ledger consumer lag > 5000 | ticket | `runbooks/kafka-lag.md` |
| `PGGatewayP99LatencyHigh` | gateway p99 > 300ms 持續 10m | ticket | `runbooks/latency.md` |
| `PGGateway5xxRateHigh` | 5xx > 1% 持續 5m | page | `runbooks/5xx.md` |
| `PGGrpcErrorRateHigh` | 非用戶端錯誤碼 > 5% | page | 同上 |
| `PGServiceDown` | target down 2m | page | `runbooks/service-down.md` |
| `PGDBPoolSaturated` | pool 使用 > 90% | ticket | `runbooks/db-pool.md` |
| `PGIdempotencyStoreErrors` | Redis 錯誤 > 1/s | page | `runbooks/redis.md` |
| `PGReconciliationMismatch` | 1h 內有差異 | ticket（finance） | `runbooks/reconciliation.md` |

### 8.3 Runbook 索引（`docs/runbooks/`，Phase 1 填寫）

每份 runbook 固定段落：影響範圍 → 快速確認指令（Grafana 連結、`kubectl`、SQL）→ 緩解步驟 → 根因排查 → 事後追蹤。

- `outbox-lag.md`：確認 Kafka 可達 → 看 relay log → 檢查 `outbox` 表 `FOR UPDATE` 鎖 → 手動調大 batch / 副本。
- `webhook-dead-letter.md`：查商戶端點回應 → 通知商戶 → `POST /admin/webhooks/{id}/replay`。
- `provider-degraded.md`：查 PSP status page → 在 routing rules 手動停用 → 觀察 failover。
- `ledger-imbalance.md`：**凍結 ledger 寫入**（feature flag）→ 找出 journal → 用反向分錄沖銷 → 事故報告（PCI/稽核）。
- `kafka-lag.md`、`latency.md`、`5xx.md`、`service-down.md`、`db-pool.md`、`redis.md`、`reconciliation.md`、`rollback.md`（helm rollback + migrate down 決策樹）、`secret-rotation.md`（API key / webhook secret 雙金鑰輪替、Vault 動態 DB 憑證）。

On-call：PagerDuty 單一 schedule（payments team），`severity=page` 進 PagerDuty，`ticket` 進 Jira，`info` 只進 Slack。

## 9. 成本估算（初期規模：500 TPS 峰值、AWS ap-northeast-1、月費 USD，約略）

| 項目 | 規格 | 月費 |
|---|---|---|
| EKS 控制平面 | 1 cluster | ~73 |
| Worker nodes | 6 × m6i.xlarge（3 AZ，on-demand；約 24 vCPU/96 GiB，實際負載 ~40%） | ~900 |
| RDS PostgreSQL Multi-AZ | db.r6g.large（2 vCPU/16 GiB）+ 200 GiB gp3 + PITR | ~550 |
| MSK | 3 × kafka.m5.large + 300 GiB | ~650 |
| ElastiCache Redis | 2 × cache.r6g.large（primary + replica） | ~330 |
| NLB + 流量 | 1 NLB、~2 TB egress | ~200 |
| 可觀測性 | 自建 Prometheus/Grafana/Jaeger/Loki 在上述 node（~2 node 份額）；或 Grafana Cloud ~300 | 0–300 |
| Vault | HCP Vault Dedicated starter 或自建 3 節點（含在 node） | 0–400 |
| ECR/GHCR、S3（對帳檔、備份）、CloudWatch | | ~100 |
| **合計** | | **約 2,800–3,500 USD/月** |

省錢選項：staging 用單 AZ + 1 年 Reserved/Savings Plan（-30~40%）、非尖峰用 Spot 跑 provider-mock/recon、RDS 先單 instance 多 database。成長到 5k TPS 時主要增加 RDS（r6g.2xlarge → Aurora）、MSK 與 node 數，估 3–4 倍。

## 10. CI/CD 流程摘要

```
PR ──▶ lint ─┬─▶ build(matrix 8 services, buildx, trivy) ──▶ (main/tag) push ghcr.io
         proto│
         test ┴─▶ integration (postgres/redis services + testcontainers)
         security (govulncheck, gitleaks, trivy fs)
         helm (lint, template, kubeconform, compose config)

tag vX.Y.Z ──▶ release.yaml: images (multi-arch, cosign, SBOM) → helm package → oci push → GitHub Release
```

- `api/gen` 必須 commit；CI 會重新 `make proto` 並 `git diff --exit-code api/gen`。
- Image tag 慣例：`sha-<short>`、`commit-<sha>`（CI 內部掃描用）、`main`、`X.Y.Z`、`X.Y`、`latest`（僅正式版）。
- 所有 workflow 以最小權限 `permissions` 宣告；push 到 ghcr 只在 `main` 與 tag。

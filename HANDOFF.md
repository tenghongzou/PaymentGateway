# HANDOFF — PaymentGateway Phase 0

**日期**：2026-08-21　**狀態**：Phase 0 完成（設計 + 可編譯 monorepo + provider-mock 端到端）

## 10 分鐘上手
1. 讀 [`docs/01-architecture.md`](docs/01-architecture.md)（單一事實來源）→ [`docs/10-codebase-guide.md`](docs/10-codebase-guide.md)（程式碼導覽）。
2. `make tools && make proto && make build && go test ./...`
3. `docker compose -f deploy/compose/docker-compose.yaml up -d postgres redis kafka`，啟動 `bin/provider-mock`、`bin/payment-service`、`bin/api-gateway`，跑 `scripts/dev-pay.sh` 與 `go test -tags e2e ./test/e2e/...`。

## 交付摘要
| 類別 | 內容 | 驗證 |
|---|---|---|
| 文件 | `docs/01`–`11` + `docs/adr/0001`–`0012` | 跨文件一致性審查 |
| 契約 | Protobuf 7 service / 49 rpc、OpenAPI 3.1（27 op、14 webhook 事件） | `buf lint` / `buf build` |
| 資料庫 | 5 個 DB 的 migrations | PostgreSQL 16 up/down/不變條件 |
| 部署 | Dockerfile、compose、Helm、GitHub Actions、OTel/告警 | `compose config`、`helm lint/template` |
| 程式 | `pkg/` 14 套件 + 8 服務（api-gateway、payment、merchant、ledger、webhook、reconciliation、provider-mock、provider-stripe 骨架） | `go build/vet` ✅、`go test ./...` 36 套件 ✅、`make build` 8 binary ✅、e2e 7 案例 ✅ |

## 立即待辦
- **Lint**：`internal/{merchant,ledger,webhook,reconciliation}` 共 412 條 golangci-lint（核心套件 0）。一次只跑一個目錄，指令見 `docs/11` §2.1。
- **Schema backlog**：部分欄位暫存 jsonb，Phase 1 第一個 migration 清單見 `docs/11` §3。
- **Proto backlog**：`IngestProviderWebhook`、`LedgerEvent`、`DisputeOpened.stage` 等，見 `docs/11` §4。
- **已知風險**：簽章 canonical 無 nonce，同商戶同秒完全相同的請求會被判重放（`docs/11` §5）。

## Phase 1 目標
Stripe adapter 上線、ledger/webhook 補齊 TODO、schema backlog 落地、risk-service 前置。路線圖與 squad 分工見 [`docs/08-team-and-roadmap.md`](docs/08-team-and-roadmap.md)。

完整細節：[`docs/11-phase0-handoff.md`](docs/11-phase0-handoff.md)

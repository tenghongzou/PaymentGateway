# PaymentGateway

以 **Go 1.26** 撰寫的多商戶支付閘道（Payment Gateway）微服務系統。
提供統一的 REST API 讓商戶接入多家支付供應商（PSP），並負責付款／退款／拒付生命週期、
供應商路由與 failover、雙式記帳帳本、商戶 Webhook 通知與 PSP 對帳。

> Phase 0 已完成：設計文件 + 可編譯 monorepo（8 個服務、`go test ./...` 全綠、provider-mock 端到端）。
> 完整架構與決策請先讀 [`docs/01-architecture.md`](docs/01-architecture.md)，交付狀態與待辦見 [`docs/11-phase0-handoff.md`](docs/11-phase0-handoff.md)。

## 服務一覽

| 服務 | 對外 | 職責 |
|---|---|---|
| `api-gateway` | HTTP :8080 | 商戶 API 入口：驗簽、冪等、限流、REST→gRPC |
| `merchant-service` | gRPC :9001 | 商戶、API Key、Webhook 端點、路由偏好 |
| `payment-service` | gRPC :9002 | Payment / Refund / Dispute 狀態機、路由、Saga orchestrator |
| `ledger-service` | gRPC :9003 | 雙式記帳帳本、餘額 |
| `webhook-service` | gRPC :9004 | 對商戶的事件通知、簽章、重試 |
| `reconciliation-service` | gRPC :9005 | PSP 結算檔對帳 |
| `provider-mock` | gRPC :9101 | 模擬 PSP（開發/測試） |
| `provider-stripe` | gRPC :9102 | Stripe adapter |

## 快速開始

```bash
make tools          # 安裝 protoc-gen-go / protoc-gen-go-grpc / golangci-lint / migrate / buf 到 ./bin
make proto          # 由 api/proto 產生 api/gen/go
make build          # 編譯所有服務到 ./bin
make test           # 單元測試
make compose-up     # 啟動 PostgreSQL / Valkey / Kafka / OTel / Jaeger / Grafana 與全部服務
```

啟動後：
- API：`http://localhost:8080/v1`（範例見 `docs/03-api.md`）
- Jaeger：`http://localhost:16686`、Grafana：`http://localhost:3000`

## 文件索引

| 文件 | 內容 |
|---|---|
| [01-architecture](docs/01-architecture.md) | 架構總覽、服務切分、技術棧、目錄結構（**單一事實來源**） |
| [02-domain-and-ledger](docs/02-domain-and-ledger.md) | 領域模型、狀態機、雙式記帳、路由決策 |
| [03-api](docs/03-api.md) | REST / gRPC API 設計、簽章、Webhook 事件 |
| [04-data-model](docs/04-data-model.md) | 各服務資料庫 schema、分割、索引 |
| [05-flows-and-sequences](docs/05-flows-and-sequences.md) | 關鍵流程序列圖、逾時預算、一致性分析 |
| [06-security-compliance](docs/06-security-compliance.md) | PCI-DSS 範圍、認證簽章、威脅模型 |
| [07-deployment](docs/07-deployment.md) | 本機開發、環境變數、K8s/Helm、CI/CD、監控告警 |
| [08-team-and-roadmap](docs/08-team-and-roadmap.md) | 團隊組織、路線圖、風險、上線檢查表 |
| [09-testing-strategy](docs/09-testing-strategy.md) | 測試金字塔、provider-mock 情境、負載測試 |
| [10-codebase-guide](docs/10-codebase-guide.md) | 程式碼導覽、如何新增 use case / provider adapter |
| [11-phase0-handoff](docs/11-phase0-handoff.md) | Phase 0 交付狀態、schema/proto backlog、TODO |
| [adr/](docs/adr/) | 架構決策紀錄 |

## 目錄結構

```
cmd/<service>/           各服務進入點
internal/<service>/      domain / app / adapter 三層（服務間不可互相 import）
pkg/                     跨服務共用套件（money、outbox、idempotency、grpcx、…）
api/proto, api/gen/go    gRPC 契約與產物；api/openapi 商戶 REST 規格
migrations/<service>/    golang-migrate 純 SQL
deploy/                  Dockerfile、docker-compose、Helm、OTel
docs/                    設計文件與 ADR
```

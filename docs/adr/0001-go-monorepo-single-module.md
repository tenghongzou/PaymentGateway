# ADR-0001：採用 Go monorepo 單一 `go.mod`，而非多 repo / 多 module

- **Status**：Accepted（2026-08）
- **Deciders**：後端架構組
- **關聯**：`01-architecture.md` §3、§7；ADR-0012

## Context

系統由 8 個以上的 Go 服務（api-gateway、merchant、payment、ledger、webhook、reconciliation、provider-mock、provider-stripe，Phase 2 再加 risk）組成，彼此透過 gRPC 與 Kafka 溝通，並共用大量橫切套件（`pkg/money`、`pkg/outbox`、`pkg/grpcx`、`pkg/sig`…）以及一份 Protobuf 契約。

團隊初期規模小（< 10 人），需求變動頻繁，一個功能經常需要同時改 proto、payment-service、ledger-service 與 gateway。我們需要決定程式碼的組織方式。

候選：

1. 每個服務一個 git repo，共用套件另開 repo 並以 semver 發版。
2. 單一 git repo，但每個服務 / 共用套件各自一個 `go.mod`（Go workspace / multi-module）。
3. 單一 git repo、單一 `go.mod`（`github.com/tenghongzou/paymentgateway`）。

## Decision

採用 **選項 3：單一 repo、單一 `go.mod`**。

- Module path：`github.com/tenghongzou/paymentgateway`。
- 服務進入點放 `cmd/<service>/main.go`；服務私有程式碼放 `internal/<service>/`；跨服務共用放 `pkg/`；契約放 `api/`。
- 以 **import 規則** 取代 module 邊界來維持服務隔離：
  - `internal/<a>` 不得 import `internal/<b>`。
  - `domain` 不得 import `app` / `adapter`；`app` 不得 import `adapter`。
  - 服務之間只透過 `api/gen/go`（gRPC client）或 Kafka 事件互動。
- 以 CI 的靜態檢查（`golangci-lint` 的 `depguard` 規則 + 自寫的 `go list -deps` 腳本）強制執行上述規則；違規即 build 失敗。
- 一份 `go.sum`、一份依賴版本：所有服務使用同版 `grpc`、`pgx`、`otel`。
- 每個服務可獨立建置與部署：Dockerfile 以 `--build-arg SERVICE=payment-service` 指定；CI 以 `go build ./cmd/...` 一次驗證全部可編譯，但只 push 有變更的 image（以 `git diff` 路徑判斷）。

## Consequences

### 正面

- **原子變更**：proto 改動與所有受影響服務的更新在同一個 PR、同一個 commit，CI 一次驗證，不存在「契約 repo 發版 → 各服務 bump 版本」的多步流程與版本漂移。
- **重構成本低**：IDE 重新命名、`gofmt -r`、`go vet ./...` 覆蓋整個系統。
- **單一依賴圖**：不會出現兩個服務用不同版本的 `pgx` 造成行為差異；安全更新只需改一處。
- **新人上手**：`git clone` + `make up` 即可跑起全系統。
- **測試共享**：`testcontainers` helper、fake adapter、fixture 都在同一棵樹。

### 負面 / 代價

- **CI 時間隨 repo 成長**：需要以路徑為基礎的 test selection 與 Go build cache（見 `09-testing-strategy.md` §7）。
- **邊界靠紀律與 lint 維持**，不像 module 邊界是編譯器強制；因此 `depguard` 規則是必要的，不是可選的。
- **一個 `go.mod` 意味著一個服務需要的重量級依賴（例如 Stripe SDK）會出現在所有服務的依賴圖中**。緩解：Go 只連結實際 import 的套件，binary 不會膨脹；但 `go mod download` 與供應鏈掃描範圍會擴大。PSP SDK 仍限制只能在 `internal/provider-<psp>/` import（`depguard`）。
- **權限粒度**：無法以 repo 權限區隔誰能改哪個服務；以 `CODEOWNERS` 做 review 強制。
- **Git 歷史與 PR 噪音**：以 conventional commit scope（`feat(payment): …`）與 PR label 緩解。

### 何時重新評估

- 團隊超過 ~40 人、或出現需要獨立發版節奏的外部消費者（例如把 `pkg/money` 開源）時，可將個別 `pkg/` 抽成獨立 module；屆時 import path 不變（仍是 `github.com/tenghongzou/paymentgateway/pkg/money`），僅新增 `go.mod`。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| 多 repo | 契約與共用套件需要發版與 bump，一個功能橫跨 3–4 個 PR；跨 repo 的 CI 無法在合併前驗證整合；小團隊負擔過重。 |
| 單 repo 多 module（`go.work`） | 保有原子變更，但仍需管理 module 間的 `replace` / 版本，`go.work` 在 CI 與 Docker build 中的行為易出錯；`go test ./...` 不跨 module；實際收益（更嚴格的邊界）可用 `depguard` 以更低成本達成。 |
| Bazel / Pants 驅動的 monorepo | 精準的增量建置與測試，但學習曲線與維護成本對 < 10 人團隊不划算；Go 原生工具鏈 + build cache 已足夠。Phase 3 若 CI 時間失控再評估。 |

## 對 01 文件的影響

無需修改 01：01 §3「單一 `go.mod` monorepo」與 §7 的 import 規則即為本決策；本 ADR 補充執行手段（`depguard`、CODEOWNERS、路徑式 CI 選擇）。

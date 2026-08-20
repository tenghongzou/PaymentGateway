# ADR-0012：Protobuf 產物（`api/gen/go`）commit 進 repo

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §3、§7；ADR-0001、ADR-0010

## Context

所有服務間契約（gRPC 服務、事件 payload）定義在 `api/proto/pg/<service>/v1/*.proto`，以 `buf` + `protoc-gen-go` + `protoc-gen-go-grpc` 產生 Go 程式碼。需要決定產生的 `.pb.go` 是：

1. 在 build 時產生（不 commit，`.gitignore` 掉），或
2. commit 進 repo（`api/gen/go/...`）。

考量：

- 開發者 `git clone` 後能否直接 `go build ./...` 與 IDE 跳轉。
- CI 與 Docker build 的可重現性與速度。
- 產生器版本（`protoc-gen-go` 1.3x vs 1.3y）差異造成的不一致。
- Code review 時能否看到契約變更的實際影響。
- 未來可能有非 Go 消費者（Phase 2 的商戶後台 TypeScript、或其他語言的 adapter）需要 `api/gen/<lang>`。

## Decision

1. **產物 commit 進 repo**：`api/gen/go/pg/<service>/v1/*.pb.go`、`*_grpc.pb.go`。`.gitattributes` 標記為 `linguist-generated=true`（GitHub PR 預設折疊）與 `-diff`（可選）。
2. **產生只透過 `make proto`**，使用 **pinned 版本**的工具鏈：`buf`、`protoc-gen-go`、`protoc-gen-go-grpc` 的版本寫在 `tools/go.mod`（`go run` 方式）或 `buf.gen.yaml` 的 remote plugins 指定版本，所有人與 CI 使用同一版本。
3. **CI 強制一致**：`make proto && git diff --exit-code api/gen` ——若有人改了 `.proto` 卻沒重新產生、或用了不同版本的產生器，CI 失敗。
4. **CI 同時執行 `buf breaking --against .git#branch=main`**：禁止破壞性變更（刪欄位、改號、改型別）；需要破壞性變更時開新版本 package（`v2`）。
5. **`buf lint`** 強制命名規範（`DEFAULT` 規則集 + `PACKAGE_VERSION_SUFFIX`）。
6. Docker build **不**安裝 protoc / buf；直接使用 repo 內的產物，映像建置更快且不依賴網路下載 plugin。
7. 非 Go 語言的產物（未來）同樣 commit 於 `api/gen/<lang>`，但是否產生由該語言消費者的需要決定。
8. Proto 檔與產物**一律同一個 PR 變更**；review 時以 `.proto` diff 為主，產物 diff 作為輔助（例如確認沒有意外的欄位號變動）。

## Consequences

### 正面

- `git clone` → `go build` 立即可用；IDE（gopls）可直接索引型別與跳轉；新人不需先裝 protoc 工具鏈。
- Build 可重現：相同 commit 一定是相同的程式碼，不受本機 / CI 上 plugin 版本影響。
- CI 與 Docker 更快、更少外部依賴（供應鏈面更小）。
- PR 中可看到產物變更，對「這個 proto 改動到底生成了什麼」一目了然（例如 oneof 改動造成的 API 變化）。
- `go mod` / `go list` / `golangci-lint` 對產物可見，depguard 規則可約束誰能 import 哪個 service 的 proto。

### 負面 / 代價

- **Repo 體積與 diff 噪音**：`.pb.go` 檔案大；以 `linguist-generated` 折疊、`git diff` 時用 `-- . ':!api/gen'` 過濾。
- **忘記重新產生**會導致不一致：由 CI 的 `git diff --exit-code` 攔截（代價是 PR 失敗一次）。
- **合併衝突**：兩個 PR 同時改同一個 proto 時，產物幾乎必衝突；解法固定為「合併 `.proto` 後重跑 `make proto`」，不要手動解產物衝突。寫進 CONTRIBUTING。
- **工具鏈版本升級**會產生一次大 diff（所有檔案的 header / 生成方式改變）；以獨立 PR 進行、不夾帶 proto 語意變更。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| Build 時產生、不 commit | 每個開發者與 CI 都需安裝相同版本工具鏈；IDE 在未產生前無法解析型別；Docker build 需下載 plugin；版本差異造成「我的機器可以 build」問題；PR 看不到實際影響。 |
| 產物發佈為獨立 Go module（`paymentgateway-api`） | 與 ADR-0001 的單 module 決策衝突；契約與實作分 repo 後會回到版本 bump 地獄。 |
| 使用 Buf Schema Registry（BSR）remote generation，`go get buf.build/gen/go/...` | 不需 commit 產物且版本由 BSR 管理，但引入對 BSR 的網路與帳號依賴（私有 repo 需付費方案）、離線 build 不可能、CI 多一個外部失敗點。Phase 2 若有多語言 / 外部消費者可評估把 proto **發佈**到 BSR 作為額外分發管道，但 repo 內仍 commit Go 產物。 |
| 只 commit proto，以 `go:generate` 在 `go build` 前自動跑 | `go build` 不會自動執行 `go generate`；仍需所有人安裝工具鏈；與「build 時產生」相同問題。 |

## 對 01 文件的影響

無需修改 01：01 §7 已標示 `api/gen/go` 為「protoc 產物（commit 進 repo）」；本 ADR 補充 pinned 工具鏈、CI `git diff --exit-code` 與衝突解法。

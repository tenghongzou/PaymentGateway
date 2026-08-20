# ADR-0010：REST 對外、gRPC 對內，不提供 GraphQL

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §3、§4、§8；`03-api.md`；ADR-0006、ADR-0012

## Context

需要決定兩組介面的協定：

- **對外**（商戶後端 → 我們）：商戶用各種語言與 HTTP client 接入，需要穩定、可版本化、容易除錯、與業界 PSP API 慣例（Stripe / Adyen 皆為 REST/JSON）一致的介面；也包含 PSP inbound webhook（PSP → 我們，只能是 HTTP）。
- **對內**（服務 ↔ 服務）：低延遲、強型別、需要 deadline 傳遞、串流（未來）、mTLS；呼叫方與被呼叫方都是我們自己的 Go 程式。

另有人提議以 GraphQL 提供對外查詢（商戶後台需要彈性查詢）。

## Decision

1. **對外：REST/JSON，OpenAPI 3.1 描述**，由 `api-gateway` 提供，路徑前綴 `/v1/`。
   - 資源導向：`POST /v1/payments`、`GET /v1/payments/{id}`、`POST /v1/payments/{id}/capture|cancel|confirm`、`POST /v1/payments/{id}/refunds`、`GET /v1/refunds/{id}`、`GET /v1/balance`、`GET /v1/balance_transactions`、`/v1/webhook_endpoints`、`/v1/webhook_deliveries/{id}/retry`。
   - 錯誤格式固定（`01` §8）；分頁用 cursor（`starting_after` / `limit`）；時間 RFC 3339 UTC；金額 integer minor unit（ADR-0004）。
   - 版本策略：URL 大版本（`/v1`）只在破壞性變更時遞增；非破壞性變更（加欄位、加端點、加列舉值）不升版，商戶必須容忍未知欄位與未知列舉值（文件化）。
   - OpenAPI 檔 `api/openapi/payment-gateway.yaml` 為**手寫**契約（不是從程式碼生成），CI 驗證 gateway 的實際回應符合 schema（`09` §2.4）。
   - PSP inbound webhook：`POST /psp/{provider}/webhook`，同樣由 gateway 接收，不在 `/v1` 命名空間、不需商戶驗證。
2. **對內：gRPC + Protobuf**，每個服務一個 `pg.<service>.v1` package；生產環境 mTLS；`pkg/grpcx` 統一 interceptor（otel、logging、recovery、deadline 檢查、錯誤轉換）。
   - api-gateway 是唯一的 REST→gRPC 轉譯點；轉譯規則（gRPC status → HTTP status、錯誤碼映射）集中在 `internal/api-gateway/adapter/http/errors.go`。
   - gRPC 錯誤以 `google.rpc.Status` + `ErrorInfo` detail 攜帶 `code` / `param`，gateway 據此組 REST 錯誤。
   - 服務間**不**走 gateway、不走 REST。
3. **事件：Protobuf over Kafka**，與 gRPC 共用同一套 proto（ADR-0003、ADR-0012）。
4. **不提供 GraphQL**（對外與對內皆不提供）。商戶後台（Phase 2）的查詢需求以**專用的 REST 列表端點 + 篩選參數 + cursor 分頁**滿足；若出現真正需要跨資源任意組合的查詢，以 BFF（backend-for-frontend）在後台前端的服務內組合 REST，而非對商戶開放 GraphQL。
5. **gRPC-Gateway / grpc-web 不用於對外**：REST 介面是刻意設計的商戶契約，不是 proto 的機械映射；兩者各自演進（REST 穩定、proto 可較頻繁調整）。

## Consequences

### 正面

- 商戶以任何語言、任何 HTTP client、`curl` 即可接入與除錯；與 Stripe/Adyen 慣例一致降低整合成本。
- 對內強型別、編譯期檢查、deadline 與 trace 自動傳遞、二進位高效；proto 是單一契約來源。
- REST 契約與內部 proto 解耦：內部重構不外洩給商戶。
- 少一種技術（GraphQL）的學習、工具鏈、安全面（query depth / complexity 攻擊、N+1、快取困難）。

### 負面 / 代價

- **兩套契約要維護**（OpenAPI + proto）與一層轉譯程式碼；以 OpenAPI 驗證測試與錯誤映射表的單元測試防止漂移。
- REST 的列表查詢彈性有限；新查詢需求要加端點或參數（但這也讓效能與索引可控）。
- 商戶後台若需要「一頁多資源」，前端要多打幾個請求或由 BFF 組合。
- gRPC 的瀏覽器支援差，但我們沒有瀏覽器直連內部服務的需求。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| 對外 GraphQL | 商戶整合是 server-to-server 的寫入為主（建立付款 / 退款），GraphQL 的強項（前端彈性查詢、減少 round trip）用不上；mutation 的冪等與錯誤語意不如 REST 明確；query 複雜度控制、快取、限流、OpenAPI 式的契約測試都更難；業界 PSP 無人以 GraphQL 為主要介面。 |
| 對外也用 gRPC（或 gRPC + grpc-gateway 自動產 REST） | 商戶端工具鏈門檻高；自動生成的 REST 會把內部 proto 形狀外洩，任何 proto 調整都變成對外變更；錯誤格式與分頁慣例難以符合 REST 最佳實務。 |
| 對內也用 REST/JSON | 沒有編譯期型別、deadline 需手動傳、序列化成本高、契約靠文件；gRPC 在 Go 生態是自然選擇。 |
| 對內用訊息（Kafka request-reply）取代 gRPC | 同步請求走訊息匯流排延遲高、除錯難；事件用 Kafka、命令 / 查詢用 gRPC 的分工更清楚（ADR-0005）。 |
| 對外 REST + 對外 GraphQL 並存（僅後台） | 兩套對外契約的安全、限流、版本、文件都要雙份；後台需求以 BFF 解決更便宜。若 Phase 3 商戶後台確實需要，GraphQL 只會存在於 BFF 與前端之間，不對商戶 API 開放。 |

## 對 01 文件的影響

無需修改 01：01 §3 已定「REST 對外、gRPC 對內」；本 ADR 明文排除 GraphQL（01 未提及），建議在 01 §3 對外 API 列加註「不提供 GraphQL（見 ADR-0010）」。

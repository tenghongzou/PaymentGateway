# ADR-0006：Provider adapter 作為獨立服務，而非 in-process plugin

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §4、§4.2、§6.4；`06-security-compliance.md`；ADR-0005

## Context

payment-service 需要與多家 PSP（Stripe、Adyen、LINE Pay、ECPay…）溝通。每家 PSP 有自己的 SDK、認證方式、webhook 簽章、重試語意、速率限制、甚至不同的 TLS / IP allowlist 需求。

把每家 PSP 的整合程式碼放進 payment-service 行程（in-process plugin，例如 Go interface + 多個實作編譯進同一個 binary）是最直覺的做法，但會帶來：

- PSP SDK 的依賴（與其 transitive 依賴）全部進入 payment-service，一個 SDK 的安全漏洞或版本衝突影響核心服務。
- 某家 PSP 的 HTTP client 掛住、goroutine 洩漏、記憶體暴增，會拖垮所有 PSP 的付款。
- 新增或修補一個 PSP 就要重新部署 payment-service。
- PCI-DSS 範圍：雖然本系統不碰 PAN，但 adapter 會持有 PSP API secret、處理 PSP webhook 原文、並且是唯一需要對外連線到 PSP 的元件；把它們和核心狀態機放在同一個行程會擴大 CDE 邊界與稽核範圍。

## Decision

1. **每個 PSP 一個獨立的 gRPC 服務**（`provider-mock :9101`、`provider-stripe :9102`、Phase 2 `provider-adyen`、`provider-linepay`…），全部實作**同一份** proto 介面 `pg.provider.v1.ProviderAdapter`：`Authorize`、`Capture`、`Void`、`Refund`、`GetPaymentStatus`、`ParseWebhook`、`HealthCheck`。
2. **payment-service 只依賴 proto 介面**；以設定檔 / merchant 路由偏好中的 provider 名稱對應到 gRPC endpoint（`PG_PROVIDER_ENDPOINTS=stripe=provider-stripe:9102,mock=provider-mock:9101`）。新增 PSP 不需改 payment-service 程式碼。
3. **adapter 是無狀態的**：沒有 DB，不發 Kafka 事件；需要的對應關係（我們的 `payment_id` ↔ PSP 的 `payment_intent_id`）由 payment-service 持有並在每次呼叫時傳入。
4. **adapter 負責的事**：
   - 把標準請求轉成 PSP API 呼叫，並把 PSP 回應 / 錯誤**正規化**成 `ErrorClass`（`05` §0.4）。
   - 把 payment-service 給的 `psp_idempotency_key` 傳給 PSP 的冪等機制。
   - 從 Vault 取得 PSP API key 與 webhook secret；這些 secret **不會**出現在 payment-service。
   - `ParseWebhook`：驗 PSP 簽章、解析、正規化成 `ProviderEvent`；無副作用。
   - 宣告能力（`Capabilities`：支援部分 capture、自動釋放剩餘額度、非同步退款、3DS 類型…）。
   - 從 ctx deadline 推導 PSP HTTP timeout；**不自行重試**（避免與 orchestrator 的策略疊加）。
5. **adapter 不負責的事**：狀態機、路由、failover、記帳、對商戶通知、冪等（除了轉傳 key）。
6. **部署**：各 adapter 是獨立 Deployment，有自己的資源限制、HPA、NetworkPolicy（只允許 egress 到該 PSP 的網域 / IP、ingress 只來自 payment-service 與 api-gateway）。
7. **PSP SDK 的 import 限制**：`depguard` 規則只允許 `internal/provider-<psp>/` import 該 PSP 的 SDK；`internal/payment-service` 若 import 任何 PSP SDK 即 CI 失敗。
8. **provider-mock 與真實 adapter 地位相同**：同一介面、同樣部署方式，用於本機 / CI / 混沌測試（`09` §3）。

## Consequences

### 正面

- **故障隔離**：Stripe adapter OOM 或 goroutine 洩漏只影響走 Stripe 的付款；circuit breaker + failover 可切到其他 PSP。
- **獨立部署**：修 Stripe SDK 升級、換 API 版本、加 Adyen，都不動 payment-service。
- **安全邊界清楚**：PSP secret、PSP webhook 原文、對外 egress 只存在於 adapter；NetworkPolicy 可精確限制；稽核時可指出「只有這些 pod 會跟 PSP 講話」。
- **可獨立測試**：每個 adapter 有自己的契約測試（對 PSP sandbox）與 record/replay 測試；payment-service 用 provider-mock 或 in-memory fake 即可完整測試 Saga。
- **多語言彈性**：若某 PSP 只有 Java / Node SDK，adapter 可用其他語言實作，只要遵守 proto。

### 負面 / 代價

- **多一次網路 hop**：payment-service → adapter 的 gRPC 往返（同叢集內 < 1ms，相對 PSP 的 100–1000ms 可忽略），以及多一層 deadline 傳遞需要正確實作（`05` §11.2）。
- **更多 Deployment 要維運**：每個 PSP 一個；以 Helm chart 模板化（`deploy/helm/paymentgateway/templates/provider-adapter.yaml` 以 values 列表展開）。
- **介面演進成本**：新增 PSP 能力（例如 Phase 3 的 payout）需要改共用 proto 並讓所有 adapter 回應（可回 `UNIMPLEMENTED`）。proto breaking check 保護相容性。
- **正規化的損耗**：PSP 特有的欄位需要透過 `metadata map<string,string>` 或 `raw_ref` 透傳；過度正規化會丟資訊，過少則讓 payment-service 認識 PSP 細節。原則：`ErrorClass` 與狀態必須正規化；其餘放 `provider_metadata` 供稽核與除錯，payment-service 不得依其做決策。
- **本機開發**需要多跑幾個行程；docker-compose 預設只起 provider-mock，真實 adapter 以 profile 開啟。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| in-process plugin（Go interface + 編譯進 payment-service） | 最低延遲與最少部署單位，但無故障隔離、無獨立部署、secret 與 SDK 進入核心服務、CDE 邊界擴大；一家 PSP 的問題成為全系統問題。 |
| Go `plugin` 套件 / WASM 動態載入 | 仍在同一行程內，隔離性與 in-process 無異，且 Go plugin 的建置與版本限制很多。 |
| 通用 adapter 服務（一個服務內含所有 PSP，以設定切換） | 比 in-process 好一點（與核心分離），但 PSP 之間仍互相影響、SDK 依賴混在一起、無法針對單一 PSP 設定 NetworkPolicy 與資源限制。 |
| Sidecar（每個 payment-service pod 旁掛 adapter 容器） | 隔離行程但不隔離部署與擴展；adapter 數量 × payment-service 副本數的資源浪費；PSP 連線數倍增。 |

## 對 01 文件的影響

無需修改 01：01 §4 / §4.2 已把 adapter 定為獨立服務；本 ADR 補充 adapter 的職責邊界、`Capabilities`、NetworkPolicy 與 SDK import 限制。

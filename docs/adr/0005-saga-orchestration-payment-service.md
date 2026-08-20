# ADR-0005：Saga 採 orchestration（payment-service 為 orchestrator），而非 choreography

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §6.3；`05-flows-and-sequences.md` §1–§5；ADR-0003

## Context

一筆付款橫跨多個步驟與多個系統：建立聚合根 → 路由 → 對 PSP Authorize（可能 3DS、可能 failover）→ Capture → 記帳 → 通知商戶。每一步都可能失敗或結果不明，且需要補償（Void、釋放退款預留額度）。

兩種 Saga 風格：

- **Choreography**：沒有中央協調者，每個服務監聽事件並決定下一步。例如 `payment.created` → routing-service 發 `provider.selected` → provider adapter 發 `authorization.succeeded` → payment-service 更新狀態 → …
- **Orchestration**：一個服務（orchestrator）明確驅動整個流程，以同步呼叫或命令訊息指揮參與者，並負責補償。

支付流程的特性：步驟之間有**嚴格順序與條件分支**（failover、3DS、manual capture）、需要**在一次 API 呼叫內回傳同步結果**（商戶要立刻知道成功 / 拒絕 / 需要 3DS）、失敗補償邏輯複雜且必須可稽核。

## Decision

1. **payment-service 是付款 / 退款 / 爭議 Saga 的唯一 orchestrator**。它擁有狀態機，並**同步**（gRPC）呼叫 provider adapter 執行 Authorize / Capture / Void / Refund。
2. **核心路徑（建立 → authorize → capture）是同步的**：在商戶的 API 請求生命週期內完成，結果直接回傳。這是 P99 < 300ms（不含 PSP）與「商戶要同步知道結果」的要求所決定的。
3. **非核心路徑（記帳、通知、對帳投影）是非同步的**：payment-service 透過 outbox 發佈**事實事件**（`payment.captured`），ledger / webhook / recon 各自消費。這些服務**不會**回頭影響 payment 狀態——它們是事件的「觀察者」而不是 Saga 參與者。
4. **Saga 狀態就是 payment 狀態**：不另建 saga 表；`payments.status` + `payment_attempts` + `payment_events` 已完整表達流程進度，崩潰後由 resolver job 依狀態接手（`05` §14）。
5. **補償與收斂由 orchestrator 執行**：Capture 明確失敗 → payment 維持 `authorized`、清 `pending_operation`、回錯誤讓商戶重試（不自動 void）；退款 PSP 失敗 → 釋放 `amount_refund_pending`；逾時 → `GetPaymentStatus` 查證後決定（authorize 逾時最長 1h，之後 `failed(provider_timeout)` 交對帳兜底）。補償動作同樣寫 `payment_events`。
6. **routing 是 payment-service 內的模組**（`internal/payment-service/app/routing`），不是獨立服務；Phase 2 的 risk-service 以**同步 gRPC 查詢**的方式被 orchestrator 呼叫（`Assess(payment) → allow / review / block`），仍由 payment-service 做決定。
7. **provider adapter 是無狀態的參與者**：不持有流程狀態，不發業務事件；它只回答「這次呼叫 PSP 的結果」並把 PSP webhook 正規化（`ParseWebhook`）後交回 orchestrator。
8. **PSP inbound webhook 也是 orchestrator 的輸入**：經 adapter 正規化後由 `HandleProviderEvent` 套用到狀態機，與同步路徑共用同一組轉移規則與樂觀鎖。

## Consequences

### 正面

- **流程一目了然**：讀 `internal/payment-service/app/create_payment.go` 就能看懂完整 Saga，不需要追蹤 5 個服務的事件訂閱關係。
- **同步結果**：商戶一個 API 呼叫得到最終或中繼狀態（captured / failed / requires_action）。
- **補償集中**：所有「出錯怎麼辦」在一個地方，容易測試（table-driven 狀態機測試，`09` §2）與稽核。
- **避免循環依賴與事件風暴**：choreography 常見的「誰該對這個事件反應」與「事件順序」問題不存在。
- **新增步驟（risk 檢查）只改 orchestrator**，不需要讓所有參與者理解新事件。

### 負面 / 代價

- **payment-service 是流程知識的集中點**，程式碼會比其他服務大；以 use case 檔案分割（`create_payment`、`confirm_payment`、`capture_payment`、`void_payment`、`create_refund`、`apply_provider_event`）與共用的 `transition` helper 控制複雜度。
- **同步呼叫使 payment-service 的延遲包含 PSP 延遲**：以 deadline 預算（`05` §11）、circuit breaker、不在持鎖期間呼叫 PSP 來控制影響面。
- **payment-service 的可用性是系統可用性的上限**：需要多實例、PDB、快速 readiness。但 ledger / webhook 不可用不影響收單（它們是非同步觀察者），這是刻意的取捨。
- **orchestrator 崩潰於步驟之間**：依賴「每步先 commit 再呼叫外部」與 resolver job 接手；沒有獨立的 saga log 意味著 resolver 必須能從 `payments` + `payment_attempts` 推斷下一步，這些推斷規則必須有測試覆蓋。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| Choreography（純事件驅動） | 同步 API 回應需要 request-reply over Kafka 或長輪詢，延遲與複雜度都高；failover / 3DS 的條件分支散落在多個服務；補償邏輯難以追蹤；除錯需要跨服務重建事件時間線。對「流程有明確順序與擁有者」的支付場景不適合。 |
| 獨立的 saga-orchestrator 服務（Temporal / Cadence / 自建 workflow engine） | Temporal 對長時間、多步驟、需要人工介入的流程（例如 KYC、payout 審核）很適合，但付款核心路徑是毫秒級同步流程，引入 workflow engine 的 RTT 與營運成本不划算。Phase 3 的 payout / 結算流程可評估 Temporal；付款 Saga 維持在 payment-service。 |
| 在 api-gateway 做 orchestration | gateway 應保持無狀態、只做協定轉譯與橫切關注點；把業務流程放在邊界層會讓它無法獨立於業務變更部署，且 gateway 沒有 DB 無法記錄 Saga 進度。 |
| 把 ledger 記帳放進同步路徑（orchestrator 呼叫 ledger gRPC） | 帳本故障會直接造成收單失敗；帳本寫入量大且可批次，非同步更合適；事件已保證不遺失（ADR-0003）。 |

## 對 01 文件的影響

無需修改 01：01 §6.3 已指定 payment-service 為 orchestrator；本 ADR 補充「核心路徑同步、觀察者非同步」的邊界與 Phase 2 risk-service 的接入方式（同步 gRPC 查詢）。

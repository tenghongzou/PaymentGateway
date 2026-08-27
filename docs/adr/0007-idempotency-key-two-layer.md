# ADR-0007：冪等鍵設計 —— gateway Redis + 服務層唯一索引雙層

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §6.1；`05-flows-and-sequences.md` §10；`pkg/idempotency`

> **註（2026-08-27）**：依 ADR-0013，第一層儲存自 Redis 改為 Valkey（wire-compatible，設計不變）。本文為歷史紀錄不改寫，文中 Redis 皆讀作 Valkey。

## Context

商戶的網路重試、SDK 自動重試、以及使用者重複點擊，都會讓同一個「建立付款」請求抵達多次。支付系統必須保證**同一個意圖只執行一次**，並讓重試者得到與原始請求相同的結果。

需求：

- 所有寫入 REST 端點（`POST /v1/payments`、`/capture`、`/void`、`/refunds`、`/confirm`、`/webhook_deliveries/{id}/retry`…）都要冪等。
- 同 key 不同 payload 必須被拒絕（防止商戶誤用 key）。
- 處理中的重複請求不能造成雙重執行。
- 系統崩潰（gateway、payment-service、Redis）的任何時點都不能造成重複付款。
- 不能因為冪等機制讓 P99 延遲顯著上升。

只靠一層都有缺口：

- **只靠 Redis**：Redis 資料遺失（failover、記憶體淘汰、TTL 過期）後，同 key 會重複建立付款。Redis 不是持久化的真相來源。
- **只靠 DB 唯一索引**：無法快取回應（重試會打到服務層）、無法在處理中攔截並發重複（第二個請求會等第一個 tx 完成，佔用連線）、無法在 gateway 層就比對 payload。

## Decision

採用**雙層**設計，兩層職責不同、互為備援：

### 第一層：api-gateway + Redis（快速路徑、回應快取、並發保護）

1. 鍵：`idem:{merchant_id}:{idempotency_key}`；值：`{state: processing|done, request_hash, status_code, headers, body, created_at}`。
2. 進入：`SET key {processing, hash} NX EX 30`。取得 → 轉發上游；未取得 → `GET` 判斷：
   - `done` 且 hash 相同 → 回放（`Idempotent-Replayed: true`）。
   - hash 不同 → `409 idempotency_key_reuse`。
   - `processing` → `409 idempotency_in_progress` + `Retry-After: 1`（**不等待**）。
3. 完成：上游回 2xx / 4xx → `SET key {done, ...} EX 86400`（24h）；上游回 5xx / 逾時 / `UNAVAILABLE` → `DEL key`（不快取失敗，讓商戶能重試）。
4. `request_hash = sha256(method + path + canonical_json(body))`。
5. Redis 不可用 → **fail-closed** 回 `503`（不在沒有並發保護下轉發寫入）。
6. gateway 以 gRPC metadata `x-pg-idempotency-key`、`x-pg-request-hash` 把 key 與 hash 傳給服務層。

### 第二層：服務層唯一索引（真相來源、最後防線）

1. `payments (merchant_id, idempotency_key)` 與 `refunds (merchant_id, idempotency_key)` 唯一索引；資料列同時儲存 `idempotency_request_hash`。
2. `CreatePayment` / `CreateRefund` 在 INSERT 時若唯一索引衝突：讀回既有列 → hash 相同回 `ALREADY_EXISTS`（附既有資源；gateway 轉成 `200 + Idempotent-Replayed`）；hash 不同回 `FAILED_PRECONDITION`（gateway 轉 `409`）。
3. 對既有資源的操作（capture / cancel / confirm）的冪等由**狀態機 + `version` + `pending_operation` 互斥旗標 + `capture_idempotency_key`**（02 §8.3）自然達成：同 key 的重複 capture 回既有結果、不再呼叫 PSP；另一個 capture/void 進行中 → `409 operation_in_progress`。這類操作的 key 仍會經過第一層（快取回應、擋並發），但服務層不另建表。
4. **保留期永久**（隨資料列）；所以 24h 後 Redis 過期，同 key 仍回同一筆 payment。
5. 對 PSP 的呼叫另有 **第三層**：每個 attempt / capture / refund 帶 `psp_idempotency_key` 給 PSP（ADR-0006），保證我們重送時 PSP 不重複扣款。這不是商戶冪等鍵的一部分，但是整條鏈「恰好一次」的必要環節。

### 規格

- `Idempotency-Key` 必填於所有寫入端點；必須為 UUID（否則 `400 idempotency_key_invalid`）；缺少 → `400 idempotency_key_missing`（錯誤碼依 02 §10）。
- 回放時保留原 status code 與 body，加 `Idempotent-Replayed: true` header。
- 快取 4xx（商戶重送同樣錯誤的請求得到同樣的錯），不快取 5xx / 429 / 409 `idempotency_in_progress`。
- `pkg/idempotency` 提供 gateway middleware 與服務層 helper（`OnConflict(ctx, tx, merchantID, key, hash, loadExisting)`）。

## Consequences

### 正面

- **崩潰任何時點都安全**：gateway 崩潰 → 鎖 30s 後釋放、服務層攔截；Redis 遺失 → 服務層攔截；payment-service 崩潰於 PSP 呼叫中 → payment 已存在（`created`）、重試回既有列、resolver 接手。
- **快速回放**：重試不打到服務與 DB；P99 幾乎不受影響（一次 Redis RTT）。
- **並發重複**在 gateway 就被擋下，不佔用 payment-service 連線與 DB 鎖。
- **payload 比對**在兩層都有，誤用 key 會被發現。

### 負面 / 代價

- **兩份邏輯要保持一致**（hash 演算法、錯誤碼對應）；以 `pkg/idempotency` 單一實作 + 共用測試向量避免漂移。
- **Redis 成為寫入路徑的硬依賴**（fail-closed）；需要 Redis HA（Sentinel / Cluster）。這是刻意的：寧可短暫拒絕寫入也不冒重複付款的風險。
- **回應快取占記憶體**：body ≤ 64KB、24h TTL；以 500 TPS × 86400s × 平均 2KB ≈ 86GB 的最壞估計顯然過高 → 實際寫入量遠低於讀取、且 Redis 用 LRU 淘汰 `done` 鍵是可接受的（淘汰後落到服務層）。`processing` 鍵不得被淘汰：以 `maxmemory-policy volatile-lru` + 監控 `evicted_keys`。
- **「處理中 → 409」而非等待**：部分商戶 SDK 預期 Stripe 式的等待行為；文件需明確說明 `Retry-After`。另注意重送必須以新的 `X-Timestamp` 重新簽章，否則會先被 06 §3.3 的重放防護以 `401 signature_replayed` 拒絕。v2 可加 long-poll（上限 5s）作為選項。
- **服務層唯一索引永久保留** 意味著商戶永遠不能重用 key 建立不同的付款；這是預期行為（Stripe 為 24h，我們更嚴格），文件化即可。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| 只用 Redis | Redis 不是持久真相；failover / 淘汰 / TTL 後重複建立付款，對支付不可接受。 |
| 只用 DB 唯一索引 | 無回應快取、無並發攔截（第二個請求在 DB 等鎖）、gateway 層無法比對 payload；延遲與連線占用都差。 |
| gateway 用 PostgreSQL（而非 Redis）存冪等表 | 持久，但 gateway 就有了 DB（違反其無狀態設計），且每次寫入多一次同步 DB 寫入；Redis 的角色本來就是「快速、可重建」的第一層，真相在服務層。 |
| 服務層以獨立 `idempotency_keys` 表（而非業務表的唯一索引） | 通用但多一張表與一次寫入；業務表唯一索引已足夠且與資料列生命週期一致。對沒有自然業務列的操作（例如 `retry`）可由第一層覆蓋。 |
| 處理中的重複請求等待（long-poll）而非 409 | 對商戶友善，但佔用 gateway 連線與 goroutine，在 PSP 變慢時會放大壓力；v1 選擇明確的 409 + `Retry-After`，把重試控制權交給商戶。 |
| 以 `(merchant_id, request_hash)` 自動去重、不要求商戶帶 key | 無法區分「合法的兩筆相同金額付款」與重試；業界慣例是顯式 key。 |

## 對 01 文件的影響

無需修改 01：01 §6.1 已定義雙層；本 ADR 補充「處理中 → 409 `idempotency_in_progress`」、5xx 不快取、Redis fail-closed 與服務層永久保留等細節，建議在 01 §6.1 末加註「（見 ADR-0007）」。

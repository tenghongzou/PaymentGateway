# ADR-0009：帳本 append-only 與沖銷策略

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §5.4、§6.5；`02-domain-and-ledger.md`；`05-flows-and-sequences.md` §9、§14；ADR-0004、ADR-0008

## Context

ledger-service 以雙式記帳記錄所有資金流：付款請款、手續費、退款、拒付、準備金。帳本是商戶餘額、撥款（Phase 3）、財務報表與對帳的依據，也是監理與稽核的對象。

帳本的資料一旦可被修改（UPDATE / DELETE），就無法證明歷史餘額的正確性，也無法回答「上個月底餘額為何是這個數字」。但實務上一定會有錯帳：手續費算錯、PSP 結算與預期不符、重複記帳、人工調整。因此需要一套**不修改歷史卻能更正**的機制。

## Decision

### 資料模型

1. `accounts(id(acct_), merchant_id nullable, type, currency, created_at)`；帳戶按 **商戶 × 類型 × 幣別** 建立（系統帳戶 `merchant_id = NULL`）。科目表以 02 §7.1 為準：01 §5.4 列出的 `merchant_payable`、`psp_receivable`、`fee_revenue`、`refund_clearing`、`chargeback_reserve` 為核心科目，02 另定義 `chargeback_fee_revenue`、`bank_cash`、`psp_fee_expense`、`settlement_suspense`、`chargeback_fee_expense` 等。
2. `journals(id, event_id, source_type, source_id, description, posted_at, reversal_of nullable, created_at)`：一次業務事件對應一筆 journal；`event_id` 唯一（冪等）。
3. `entries(id, journal_id, account_id, direction ∈ {debit, credit}, amount int64 > 0, currency, created_at)`：每筆 journal 至少兩條 entry。

### 不變條件（DB 層強制）

4. **借貸平衡**：deferred constraint trigger 在交易 commit 前驗證 `SUM(debit) = SUM(credit)` 且所有 entry 幣別相同；不平衡 → 交易失敗。應用層也驗證（更早、更好的錯誤訊息）。
5. **append-only**：ledger-service 的 DB 角色對 `journals` / `entries` 僅有 `INSERT, SELECT`；另以 `RULE` / trigger 禁止 UPDATE / DELETE（雙重保險，連 superuser 誤操作也會被 trigger 擋下，除非先 disable trigger，而那會留下稽核痕跡）。
6. **金額為正數**，方向由 `direction` 表示；負金額一律拒絕（ADR-0004）。
7. **餘額是 entries 的導出值**：`account_balances(debit_total, credit_total, last_entry_id, version)` 在過帳的**同一交易**內更新（02 §7.5，供 `GET /balance` 與 payout 檢查），`balance_snapshots` 每日 00:05 UTC 快照作為重算基準；`ledger-verifier` job 每小時抽樣、每日全量以 `SUM(entries) since snapshot` 驗證 `account_balances`，差異即 `pg_ledger_imbalance_total` +1。entries 永遠是真相，餘額表只是可驗證的衍生物。

### 沖銷（reversal）策略

8. **錯帳不修改、不刪除，以反向 journal 沖銷**：新 journal 的每條 entry 與原 journal 方向相反、金額相同、帳戶相同；`reversal_of = 原 journal_id`；`description` 記錄原因。一筆 journal **最多被沖銷一次**（`journals.reversal_of` 唯一索引）。
9. **沖銷後若需正確金額，再記一筆新的正確 journal**（沖銷 + 重記，而非「差額調整」），讓每筆 journal 都能獨立被理解。例外：對帳發現的**手續費差額**允許記「調整 journal」（`source_type = recon_adjustment`），因為原 journal 本身無誤，只是 PSP 實收不同。
10. **沖銷的觸發來源**：
    - 業務事件：`refund.succeeded`（不是沖銷，是正常反向資金流，記自己的 journal）、`dispute.lost`；`refund.failed` 則以 J-REF-FAIL 沖回 J-REF-PEND（`reversal_of`）。
    - 對帳差異：ledger 消費 `reconciliation.events` 的 `discrepancy.found` 記 J-STL-DIFF（`settlement_suspense` ↔ `psp_receivable`，Δ）；人工更正以 ledger 的 ops 工具記 J-REV 沖銷 + 補正確 journal（需雙人核可、`actor` 與 `discrepancy_id` 寫入 `journals.description` 與稽核日誌）。reconciliation-service **不**直接呼叫 ledger 做調整（06 §6.2 呼叫矩陣只允許 `PostSettlement` / `GetJournalsByPayment`）。`settlement_suspense` 必須在月結前清零。
    - 系統偵測到重複記帳（同一 `event_id` 被唯一索引擋下，不會發生；但不同 `event_id` 指向同一 `source_id` 的重複由 `ledger-integrity-check` 找出，人工決定沖銷）。
11. **沖銷不能跨期關帳**：Phase 3 引入 `accounting_periods` 後，已關帳期間的 journal 只能在當期以沖銷 journal 更正，`posted_at` 為當期。

### 業務事件 → 分錄（摘要，範本 ID 與完整規則見 02 §7.3）

| 範本 | 觸發事件 | 借（Dr） | 貸（Cr） |
|---|---|---|---|
| — | `payment.created` / `authorized` / `requires_action` | 不記帳（授權不是資金移動） | |
| J-CAP | `payment.captured` | `psp_receivable`（gross） | `merchant_payable`（net）、`fee_revenue`（fee） |
| J-REF-PEND | `refund.pending` | `merchant_payable` | `refund_clearing` |
| J-REF-OK | `refund.succeeded` | `refund_clearing` | `psp_receivable`（另依政策 J-REF-FEE-RET / J-REF-FEE） |
| J-REF-FAIL | `refund.failed` | `refund_clearing` | `merchant_payable`（`reversal_of` → J-REF-PEND） |
| J-CB-OPEN | `dispute.opened`（stage ≥ chargeback） | `merchant_payable`（amount + fee） | `chargeback_reserve`、`chargeback_fee_revenue` |
| J-CB-WON | `dispute.won` | `chargeback_reserve` | `merchant_payable` |
| J-CB-LOST | `dispute.lost` | `chargeback_reserve` | `psp_receivable` |
| J-STL | `settlement.posted` | `bank_cash`、`psp_fee_expense` | `psp_receivable` |
| J-STL-DIFF | `discrepancy.found` | `settlement_suspense` Δ | `psp_receivable` Δ（或反向） |
| J-REV | 人工沖銷（雙人核可） | 原 journal 每筆 entry 方向對調 | |

12. 手續費拆分使用 `money.Allocate` / `MulRatio`（ADR-0004），確保 `net + fee = gross` 精確成立。

## Consequences

### 正面

- **可證明的歷史**：任何時間點的餘額都能從 entries 重算並與快照比對；稽核可直接查 `reversal_of` 鏈。
- **錯誤可見而非被掩蓋**：錯帳與更正都留在帳上，有原因、有操作者。
- **冪等**：`journals.event_id` 唯一，重複事件不會重複記帳。
- **不平衡不可能 commit**：`pg_ledger_imbalance_total` 恆為 0 是 DB 約束的結果，不只是監控目標。

### 負面 / 代價

- **表只增不減**：需要分區（按 `posted_at` 月份）與冷儲存歸檔策略；餘額查詢依賴快照 + 增量（`balance_snapshots.last_entry_id` 之後的 entries）。
- **沖銷 + 重記讓 entry 數量增加**（一次錯帳產生 3 筆 journal），報表需要能折疊 `reversal_of` 鏈（提供 view `journals_effective`）。
- **人工調整需要流程與權限**（J-REV ops 工具需 RBAC 與雙人核可），否則 append-only 的保護會被「合法的調整」架空。
- **deferred trigger 的效能**：每筆 journal commit 前多一次聚合；entries 數量通常 2–4 條，成本可忽略；以批次（一個交易多筆 journal）時需注意 trigger 以 journal 為單位驗證。
- **負餘額**：商戶被拒付或退款超過餘額時 `merchant_payable` 可能為負；帳本允許（它是事實），由 Phase 3 的撥款 / 風控決定如何處理。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| 允許 UPDATE 修正錯誤 entry（加 `updated_by` 稽核欄位） | 歷史餘額不可重現；稽核無法信任；一次誤操作可能無聲改變財務數字。 |
| 軟刪除（`deleted_at`）錯誤 journal | 等同修改歷史；所有查詢都要記得過濾；餘額重算結果取決於過濾條件。 |
| 單式記帳（只記商戶餘額增減） | 無法驗證平衡、無法追蹤資金在 PSP / 手續費 / 準備金之間的流向；對帳困難。 |
| 差額調整（記一筆 `correct − wrong` 的 journal）取代沖銷 + 重記 | entry 數較少，但單看調整 journal 無法理解業務意義；保留給手續費差額這種「原 journal 正確、外部事實不同」的情況。 |
| 只維護餘額表、不保留 entries（或餘額表可獨立於 entries 被修改） | 無法驗證、無法重建歷史；02 §7.5 的 `account_balances` 是與 entries 同交易更新、並由 verifier 持續驗證的**衍生物**，不是獨立真相，與本 ADR 一致。 |

## 對 01 文件的影響

無需修改 01：01 §5.4 已要求 append-only 與反向分錄沖銷；本 ADR 補充沖銷規則（一次、`reversal_of` 唯一、沖銷 + 重記）與對帳差異的處理路徑。01 §5.4 的五個科目為摘要，完整科目表以 02 §7.1 為準。

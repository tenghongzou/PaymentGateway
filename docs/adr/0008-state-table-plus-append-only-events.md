# ADR-0008：狀態表 + append-only 事件表（Event Sourcing-lite），而非完整 Event Sourcing

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §2、§5.2；`05-flows-and-sequences.md` §0.3；ADR-0003、ADR-0009

## Context

稽核要求：每一次付款狀態轉移都必須可追溯（誰、何時、從哪個狀態到哪個狀態、原因、對應的 PSP 參考）。同時系統需要：

- 以主鍵 / 商戶 / 狀態 / 時間快速查詢付款（列表 API、對帳、排程工作）。
- 以樂觀鎖保護並發轉移。
- 讓新進工程師與 DBA 能直接用 SQL 看懂資料。

完整 Event Sourcing（ES）把事件流當作唯一真相、狀態由重放事件得出，能天然滿足稽核，但也帶來快照、投影重建、事件版本升級（upcasting）、查詢只能透過讀模型等一整套基礎設施需求。

## Decision

採用 **「狀態表 + append-only 事件表」** 的混合模式：

1. **狀態表是當前真相**（`payments`、`refunds`、`payment_attempts`）：以正常的關聯式 schema 儲存當前狀態，可直接查詢、建索引、加 CHECK 約束。
2. **事件表是不可變的稽核軌跡**（`payment_events`）：每次轉移 INSERT 一列 `(id(evt_), aggregate_type, aggregate_id, seq, type, payload protobuf, occurred_at)`（02 §2.2 / 04 §2.4），`seq` = 轉移後的 `payments.version`，`(aggregate_id, seq)` 唯一；`from_status / to_status / actor / reason / provider_ref / trace_id` 放在 protobuf payload 內；表按月分割；DB 角色對此表**只有 INSERT / SELECT**，另以 trigger 拒絕 UPDATE / DELETE。
3. **兩者在同一個交易寫入**，並連同 `outbox`（ADR-0003）一起 commit；由 `transition` helper 強制（`05` §0.3）。
4. **狀態機在 domain 層以程式碼定義**（`internal/payment-service/domain/payment.go` 的 `Transition(cmd) (event, error)`），是「合法轉移」的唯一來源；狀態表的 `status` 欄位有 CHECK 列舉，但合法轉移規則不在 DB。
5. **事件表的 payload 是 `pg.payment.v1.PaymentEvent`**，與 outbox / Kafka 的 payload 相同型別，保證「DB 裡的稽核事件 = 發出去的事件」。
6. **不做事件重放得到狀態**：不提供「從 `payment_events` 重建 `payments`」的正式功能；但保留一個**一致性檢查工具**（`cmd/payment-service job verify-events`）抽樣驗證「事件表最後一筆的 `to_status` == 狀態表的 `status`」，作為防守性檢查。
7. **狀態表欄位可演進**（加欄位、改索引）；事件表只加欄位、舊列不改。
8. **退款 / 爭議**屬於 payment 聚合根，事件同樣寫 `payment_events`（`type=refund.succeeded` 等），`seq` 在 payment 範圍內遞增。

## Consequences

### 正面

- **查詢簡單**：列表、篩選、對帳、排程工作都是直接的 SQL，不需讀模型。
- **約束可靠**：金額不變條件、唯一索引、外鍵都在狀態表上生效（完整 ES 的不變條件只能在應用層）。
- **稽核完整**：每次轉移有紀錄，包含 trace_id，與 Kafka 事件一對一。
- **團隊認知負擔低**：關聯式模型 + 一張 append-only 表，DBA 與工程師都熟悉。
- **與 outbox 天然結合**：同一筆 protobuf 同時是稽核紀錄與對外事件。

### 負面 / 代價

- **不是完整的時間旅行**：無法「重放到任意時間點的狀態」；但 `payment_events` 足以回答「某時刻的狀態是什麼」（取該時間前最後一筆的 `to_status`），只是不能重建所有欄位。
- **狀態表與事件表理論上可能不一致**（程式 bug 繞過 `transition`）：以 helper 強制、code review、`verify-events` 抽樣檢查與 `pg_state_event_mismatch_total` 告警緩解。
- **事件表增長**：每筆付款 3–6 列；以 `created_at` 做 range partition（每月）便於歸檔；保留期依法規（至少 5 年）歸檔到冷儲存。
- **schema 演進**：事件 payload 欄位只增不改（protobuf 慣例），讀舊事件時新欄位為零值；沒有 upcaster 基礎設施。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| 完整 Event Sourcing（事件流為唯一真相 + CQRS 讀模型） | 稽核與重放能力最強，但需要快照、投影、upcasting、讀模型最終一致等基礎設施；DB 層無法表達不變條件（超退檢查要靠應用層）；列表查詢必須等投影；團隊與工具鏈成本高。對本系統，「狀態表 + 事件表」已涵蓋稽核需求的 95%。 |
| 只有狀態表 + 通用 audit log（trigger 寫 JSON diff） | 最省事，但 audit log 與業務語意脫節（只有欄位 diff，沒有「為什麼」與 PSP 參考），無法與 Kafka 事件對齊，也不適合作為 outbox 來源。 |
| 只有事件表（Kafka 為真相，DB 僅快取） | Kafka 不適合作為需要隨機查詢與約束的主儲存；retention 與合規保留期衝突。 |
| 使用第三方 ES 框架（EventStoreDB、Axon） | 新增一個有狀態的基礎設施元件與學習成本；與 PostgreSQL 為中心的技術棧不一致。 |

## 對 01 文件的影響

無需修改 01：01 §2「Event Sourcing-lite」即為本決策；本 ADR 補充 `seq = version`、`transition` helper 與 `verify-events` 檢查。

# ADR-0003：Transactional Outbox + Kafka 取代分散式交易 / 2PC

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §6.2；`05-flows-and-sequences.md` §8；ADR-0002、ADR-0005、ADR-0011

## Context

payment-service 每次狀態轉移都要（a）寫 `payments` 與 `payment_events`，（b）讓 ledger-service 記帳、webhook-service 通知商戶、reconciliation-service 更新投影。（a）與（b）橫跨不同資料庫與 Kafka，沒有任何單一交易能涵蓋。

典型的錯誤做法是「先 commit DB，再 publish Kafka」：兩步之間崩潰會導致事件遺失（付款成功但帳本永遠沒記）；反過來「先 publish 再 commit」則會產生幽靈事件。雙寫問題（dual write）是支付系統最常見的資料不一致來源。

候選：

1. XA / 2PC（PostgreSQL prepared transactions + Kafka transactions 的某種協調）。
2. Transactional Outbox：事件與業務資料寫同一個 DB 交易，由 relay 非同步送到 Kafka。
3. Listen-to-yourself / Event-first：先寫 Kafka，自己消費後再寫 DB。
4. CDC（Debezium 讀 WAL 直接產生事件）。

## Decision

採用 **Transactional Outbox（選項 2）**，以 **polling relay** 實作，並搭配 **冪等消費者**：

1. 每個擁有 DB 的服務都有 `outbox` 表：`(id uuid — 即全域 event_id（UUIDv7）, aggregate_type, aggregate_id, event_type, payload bytea, headers jsonb, created_at, published_at, attempts, last_error)`（04 §7）。
2. 業務寫入與 `INSERT INTO outbox` **必須**在同一個交易。`pkg/outbox.Writer` 只接受 `pgx.Tx`，不接受 pool，從型別上防止在交易外寫 outbox。
3. `pkg/outbox.Relay` 在每個服務實例內以 goroutine 執行：`SELECT ... WHERE published_at IS NULL ORDER BY created_at LIMIT 500 FOR UPDATE SKIP LOCKED` → 批次 produce（`acks=all`、`enable.idempotence=true`、key = `aggregate_id`）→ `UPDATE published_at`。輔以 PostgreSQL `LISTEN/NOTIFY` 降低延遲，100ms polling 兜底。
4. 語意是 **at-least-once**；`outbox.id`（UUIDv7）就是事件的全域 `event_id`，下游 `processed_events.event_id`、`journals.event_id`、`webhook_events.event_id` 都引用它。
5. **所有消費者必須冪等**：`processed_events(event_id, consumer)` 主鍵，與業務寫入同交易；offset 在交易 commit 後手動提交。
6. 事件 payload 為 Protobuf（`pg.<service>.v1.*Event`），與 gRPC 契約同一套 proto。
7. 已發佈的 outbox 列保留 7 天；Kafka topic retention 7 天；`processed_events` 保留 **30 天**（必須 ≥ Kafka retention，確保 retention 內的任何重送都能被去重；04 §8.3）。
8. **不使用** Kafka transactions / exactly-once semantics（EOS）：下游的副作用（DB 寫入、HTTP webhook）本來就在 Kafka 交易之外，EOS 無法涵蓋，反而增加複雜度。

## Consequences

### 正面

- **不會遺失事件、不會有幽靈事件**：事件存在 ⇔ 業務資料已 commit。
- 只依賴 PostgreSQL 的單機 ACID，沒有跨系統協調者，沒有 in-doubt 交易。
- Kafka 短暫不可用時，系統仍可收單（事件堆在 outbox），恢復後自動補送；`pg_outbox_lag_seconds` 量化積壓。
- relay 無狀態、多實例以 `SKIP LOCKED` 自然分工，不需 leader election。
- 測試容易：整合測試可直接斷言 outbox 表內容，不需啟動 Kafka。

### 負面 / 代價

- **延遲**：事件到達消費者多了一段 polling 延遲（典型 100–300ms）。對付款系統可接受（商戶 API 回應不等事件）。
- **DB 寫入放大**：每次轉移多一筆 INSERT 與一筆 UPDATE。以批次 UPDATE 與定期清理控制表大小；outbox 表只有主鍵與 `published_at` 部分索引（`WHERE published_at IS NULL`）。
- **重複投遞是常態**，所有消費者都必須做去重；這是設計約束而非選項（見 `05` §8）。
- **順序**：只保證同一 `aggregate_id` 的事件在同一 partition 有序；多 relay 實例在極端情況下可能微幅錯序（`05` §8.2 第 3 點的緩解）。消費者以事件的 `aggregate_version`（= `payment.version`）判斷新舊，舊於已處理版本者直接丟棄（02 §8.4）。
- **DLQ 與人工重放**是必須配套的營運工具。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| 2PC / XA | PostgreSQL prepared transactions 與 Kafka 之間沒有成熟的 XA 協調者；in-doubt 交易會長期持鎖；可用性與 Kafka/DB 兩者的交集；幾乎沒有現代支付系統採用。 |
| 先寫 DB 再發 Kafka（無 outbox） | 雙寫問題，崩潰視窗內事件遺失；不可接受。 |
| Listen-to-yourself（先發 Kafka 再由自己消費寫 DB） | API 回應時 DB 還沒更新，破壞 read-your-writes；錯誤處理（寫 DB 失敗時事件已發出）更難。 |
| CDC（Debezium 讀 WAL） | 同樣能保證不遺失，且不需 relay 程式碼；但引入 Kafka Connect 叢集與 Debezium 的營運負擔，事件格式被表結構綁死（需要 outbox event router 才能得到業務事件，最終仍是 outbox 表）。Phase 3 若 relay 的 polling 成為瓶頸（> 5k TPS）可切換為 Debezium outbox event router，**表結構刻意與 Debezium 的 outbox 慣例相容**（`aggregatetype`/`aggregateid`/`payload` 可直接映射），保留此升級路徑。 |
| Saga 以同步 gRPC 串接所有服務（無事件） | ledger / webhook 故障會直接讓付款失敗；耦合可用性；違反 ADR-0005 的隔離目標。 |

## 對 01 文件的影響

無需修改 01：01 §6.2 已定義 outbox 與 `processed_events`；本 ADR 補充 relay 參數、保留期（processed_events 30 天 ≥ Kafka 7 天）與不採用 Kafka EOS 的理由。

# ADR-0002：database-per-service，禁止跨庫 JOIN

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §3、§4.1；ADR-0003、ADR-0005

## Context

五個有狀態服務（merchant、payment、ledger、webhook、reconciliation）各自擁有明確的聚合根。若共用一個資料庫，最直接的誘惑是：

- reconciliation 直接 `JOIN pg_payment.payments` 與 `pg_ledger.entries` 做比對。
- webhook-service 直接查 `merchant_webhook_endpoints`。
- ledger 在同一交易裡同時更新 `payments.status` 與寫分錄，「順便」達成強一致。

這些捷徑短期省事，長期會讓 schema 變更需要全系統協調、讓某個服務的慢查詢拖垮其他服務、讓「誰擁有這張表」變得模糊，也讓 PCI / 稽核範圍難以界定。

## Decision

1. **每個有狀態服務擁有獨立的 PostgreSQL 資料庫**：`pg_merchant`、`pg_payment`、`pg_ledger`、`pg_webhook`、`pg_recon`（Phase 2：`pg_risk`）。本機 docker-compose 可跑在同一個 PostgreSQL 實例上（不同 database），生產環境可以是同一個 RDS cluster 的不同 database 或完全分開的實例；**邏輯隔離是必要的，物理隔離是可選的**。
2. **每個服務只用自己的 DB 帳號**，該帳號只對自己的 database 有權限（`GRANT CONNECT` 僅限自己的 DB）。跨庫 JOIN 在權限層就不可能。
3. **禁止** `postgres_fdw`、`dblink`、跨庫 view、共享 schema。
4. **服務需要別人的資料時，只有三條路**：
   - 同步：呼叫該服務的 gRPC API（例如 gateway 查 merchant-service 的 API key）。
   - 非同步：消費該服務發佈的 Kafka 事件，在自己的 DB 維護**本地投影**（例如 webhook-service 的 `merchant_webhook_endpoints` 投影、recon 的 `payment_records` / `ledger_postings`）。
   - 由呼叫端（api-gateway）組合多個服務的回應（API composition）。
5. **Migration 各自獨立**：`migrations/<service>/NNNN_*.sql`，由該服務自己的 init container 執行；不存在「全系統 migration」。
6. **跨服務的一致性**以 Transactional Outbox + 冪等消費者（ADR-0003）與 Saga（ADR-0005）達成，而非跨庫交易。
7. **報表 / 分析**：不得直接查生產 DB。Phase 3 以 CDC（Debezium）或事件流餵 warehouse。

## Consequences

### 正面

- 服務可獨立演進 schema、獨立擴展（ledger 的寫入量與 payment 不同）、獨立備份與復原。
- 故障隔離：recon 的大批次查詢不會鎖住 payment 的表。
- 資料擁有權清楚，PCI / 稽核範圍可按 DB 界定。
- 強迫團隊明確設計 API 與事件，避免隱性耦合。

### 負面 / 代價

- **沒有跨服務的 ACID**：付款狀態與帳本餘額之間是最終一致（可觀察視窗見 `05-flows-and-sequences.md` §12）。需要向商戶文件化。
- **資料重複**：webhook-service 與 recon 各自維護投影，需要處理投影落後、重建（replay topic）與 schema 演進。
- **查詢能力受限**：「列出某商戶所有 disputed 且餘額不足的付款」需要 API composition 或專門的投影，不能一個 SQL 解決。
- **營運複雜度**：5+ 個 DB 的監控、備份、連線池設定。以 Helm chart 模板化緩解。
- **本機開發**：docker-compose 需建立多個 database；以 `deploy/compose/init-db.sql` 一次建立。

### 強制手段

- CI 測試以各服務獨立的 testcontainers DB 執行，程式碼中不存在其他服務的連線字串。
- `pkg/pgdb` 的連線建構只接受單一 `PG_<SERVICE>_DATABASE_URL`。
- Code review checklist：「這個查詢碰到的每張表都屬於本服務嗎？」

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| 共用資料庫、以 schema 區隔 | 最終總會有人寫跨 schema JOIN；權限難以嚴格限制；慢查詢互相影響；實務上退化成分散式單體。 |
| 共用資料庫 + 跨服務交易（同 DB 內 ACID） | 短期一致性最強，但服務間透過資料表耦合，schema 變更需全體協調；與「可獨立部署」目標衝突；ledger 與 payment 的寫入負載不同，無法獨立擴展。 |
| 每服務獨立 DB，但允許 read-only 跨庫查詢（read replica / fdw） | 看似折衷，但讀取耦合一樣阻礙 schema 演進，且 fdw 效能與鎖行為難以預測。需要他人資料就走事件投影。 |
| CQRS 讀模型集中庫（所有服務的事件投影到一個查詢 DB） | Phase 3 的報表需求可以這麼做，但它是「唯讀副本」而非業務寫入路徑，不違反本 ADR。 |

## 對 01 文件的影響

無需修改 01：01 §3「database-per-service」與 §4.1 的 DB 對照即為本決策；本 ADR 補充「需要他人資料的三條路」與強制手段。

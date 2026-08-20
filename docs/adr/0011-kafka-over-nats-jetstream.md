# ADR-0011：事件匯流排選擇 Kafka（而非 NATS JetStream），以及何時可切換

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §3、§6.2；`05-flows-and-sequences.md` §8；ADR-0003；`pkg/eventbus`

## Context

Transactional Outbox（ADR-0003）需要一個持久化、可重放、支援多個獨立消費群組、按 key 保序的事件匯流排。候選：

- **Apache Kafka**（含相容實作：Redpanda、AWS MSK、Confluent Cloud）
- **NATS JetStream**
- 其他：RabbitMQ Streams、Pulsar、AWS Kinesis、Google Pub/Sub

需求：

| 需求 | 說明 |
|---|---|
| 持久化與重放 | 7 天 retention；DLQ 重放；新消費者（recon 投影重建）能從頭讀 |
| 多消費群組 | 同一事件由 ledger / webhook / recon 各自獨立消費、各自 offset |
| 按 key 保序 | 同一 payment 的事件順序 |
| 吞吐 | 500 TPS × 約 4 事件 = 2k msg/s 起，目標 20k msg/s |
| 營運 | 團隊熟悉度、託管服務可得性、監控生態 |
| 事件 schema | Protobuf payload，需要 headers |
| 與 CDC 的相容 | Phase 3 可能以 Debezium 取代 polling relay |

## Decision

1. **選擇 Kafka（協定）**。本機以 **Redpanda**（單一 binary、無 ZooKeeper、Kafka 相容）在 docker-compose 執行；生產採用託管 Kafka（AWS MSK 或 Confluent Cloud，依雲供應商決定），程式碼只依賴 Kafka 協定。
2. Go client 使用 `github.com/twmb/franz-go`（純 Go、無 cgo、支援 idempotent producer、manual commit、headers）。封裝在 `pkg/eventbus`，**應用程式碼不直接 import Kafka client**。
3. **Topic 設計**：
   - `payment.events`、`refund.events`、`ledger.events`、`merchant.events`、`reconciliation.events`；每個 topic 內以 `event_type` header 區分事件類型（消費者按需過濾），而非每種事件一個 topic（避免跨 topic 失序）。
   - partition key = `aggregate_id`；初始 12 partitions（可增不可減，增加時同 key 的歷史順序會斷，只在低峰且消費者追平後進行）。
   - 每個 topic 對應 `<topic>.dlq`。
   - retention 7 天，`cleanup.policy=delete`；`min.insync.replicas=2`、replication factor 3（生產）。
4. **Producer**：`acks=all`、`enable.idempotence=true`、`compression=zstd`、`linger.ms=5`、批次送出（outbox relay 本就是批次）。
5. **Consumer**：每個服務一個 consumer group（`ledger-service`、`webhook-service`、`reconciliation-service`）；手動 commit；`processed_events` 去重（ADR-0003）。
6. **Schema**：Protobuf；**不**引入 Schema Registry（v1），以 `schema_version` header + CI 的 proto breaking check 管理相容性；Phase 2 若有非 Go 消費者再評估 Registry。
7. **`pkg/eventbus` 的介面刻意保持最小**，以便未來切換：

   ```go
   type Publisher interface {
       Publish(ctx context.Context, msgs ...Message) error // Message{Topic, Key, Headers, Payload}
   }
   type Subscriber interface {
       Subscribe(ctx context.Context, group string, topics []string, h Handler) error
       // Handler 回 nil → ack；回 err → 重試 / DLQ；由 eventbus 管 offset
   }
   ```

## 何時可以 / 應該切換到 NATS JetStream

JetStream 的優勢在於：部署極輕（單一 binary、無外部依賴）、延遲更低（sub-ms）、內建 KV / object store、request-reply 原生、記憶體足跡小。下列情況成立時應重新評估：

- 需要在**邊緣 / 單機 / 離線**環境（例如 on-prem 商戶部署版、POS 終端閘道）運行整套系統，Kafka 的資源需求不可接受。
- 團隊確定**不會**使用 Kafka 生態（Kafka Connect / Debezium、ksqlDB、Flink、倉儲 sink connector），且託管 Kafka 成本成為顯著支出。
- 需要原生 request-reply 與大量小 topic（每商戶一個 stream）的模式。

切換成本評估（已刻意壓低）：

- 應用程式只依賴 `pkg/eventbus` 的 `Publisher` / `Subscriber`；JetStream 實作約 300–500 行。
- 語意對應：Kafka consumer group ↔ JetStream durable consumer；partition key 保序 ↔ JetStream subject 內保序 + 以 key 為 subject 後綴（`payment.events.<hash(key) % N>`）；offset commit ↔ explicit ack；DLQ ↔ max deliver + advisory。
- 需要注意：JetStream 的 ack 語意是 per-message，`processed_events` 去重設計不變；retention / replay 以 stream 設定達成；Debezium 路徑會消失。

反過來，**不應**只因為「Kafka 太重」而切換：本機用 Redpanda 已解決開發體驗問題，生產用託管服務已解決營運問題。

## Consequences

### 正面

- 成熟的持久化日誌語意、重放、多消費群組、按 key 保序，完全符合 outbox 模式。
- 生態：Debezium（Phase 3 CDC 路徑）、倉儲 sink、Grafana / kafka-exporter 監控、每家雲都有託管服務。
- 團隊與市場人才熟悉度高。
- `franz-go` 的 idempotent producer + 手動 commit 讓 at-least-once 實作直接。

### 負面 / 代價

- 生產營運比 JetStream 重（即使託管，仍要管 partition、retention、ACL、配額）；以 Terraform / Helm 模板化。
- partition 數固定後增加會破壞同 key 歷史順序；需規劃初始值（12）與擴張程序。
- 單一訊息延遲（ms 級）高於 JetStream（sub-ms），但 outbox polling 本身已是 100ms 級，不是瓶頸。
- 無 Schema Registry 時，跨語言消費者需自行取得 proto；Phase 2 再評估。

## Alternatives considered

| 選項 | 為何不選（v1） |
|---|---|
| NATS JetStream | 輕量且延遲低，但生態（CDC、sink connector、託管選項）較少；團隊對 Kafka 更熟；已以 `pkg/eventbus` 抽象保留切換路徑（見上）。 |
| RabbitMQ（classic queues） | 傳統訊息佇列語意（消費即刪）不適合重放與多獨立消費群組；Streams 功能較新、生態不如 Kafka。 |
| Apache Pulsar | 功能強（分層儲存、多租戶），但架構複雜（BookKeeper + ZooKeeper），團隊無經驗，託管選項少。 |
| AWS Kinesis / Google Pub/Sub | 雲鎖定；Kinesis 的 shard 模型與 per-shard 吞吐限制較麻煩；Pub/Sub 無 per-key 保序（需 ordering key 且限制多）。Kafka 協定在各雲皆有託管實作，可攜性較好。 |
| PostgreSQL 作為佇列（`SKIP LOCKED` 輪詢，無 Kafka） | 在 outbox 階段我們已經這麼做，但要讓多個**不同 DB** 的服務消費同一事件流，就需要一個中立的匯流排；以 DB 作為跨服務匯流排會違反 ADR-0002。 |

## 對 01 文件的影響

無需修改 01：01 §3 已選 Kafka 與 `franz-go`；本 ADR 補充 Redpanda 用於本機、topic / partition 設定、`pkg/eventbus` 最小介面與切換到 NATS JetStream 的條件。

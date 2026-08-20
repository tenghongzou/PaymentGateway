-- =============================================================================
-- merchant-service :: 0002_outbox  (database: pg_merchant)
-- Transactional Outbox + 消費端去重表（docs/01-architecture.md §6.2）
-- =============================================================================

-- -----------------------------------------------------------------------------
-- outbox — 業務資料與事件在同一交易寫入；pkg/outbox relay worker 以
--   SELECT ... WHERE published_at IS NULL ORDER BY created_at
--   FOR UPDATE SKIP LOCKED LIMIT n
-- 取出後送 Kafka（topic: merchant.events，key = aggregate_id），成功後更新 published_at。
-- 已發佈列由清理工作定期刪除（保留 7 天供除錯）。
-- -----------------------------------------------------------------------------
CREATE TABLE outbox (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),  -- = Kafka header event_id
    aggregate_type text        NOT NULL,                               -- merchant / api_key / webhook_endpoint / routing_preferences
    aggregate_id   text        NOT NULL,                               -- Kafka partition key（同聚合保序）
    event_type     text        NOT NULL,                               -- 例：merchant.created, webhook_endpoint.updated
    payload        bytea       NOT NULL,                               -- protobuf（pg.merchant.v1.MerchantEvent）
    headers        jsonb       NOT NULL DEFAULT '{}',                  -- trace context、schema version…
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,
    attempts       integer     NOT NULL DEFAULT 0,
    last_error     text
);

-- relay 撈取待送事件（partial index 只含未發佈列，極小且熱）
CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
-- 清理工作：DELETE FROM outbox WHERE published_at < now() - interval '7 days'
CREATE INDEX outbox_published_idx   ON outbox (published_at) WHERE published_at IS NOT NULL;

-- -----------------------------------------------------------------------------
-- processed_events — 消費端冪等。每個 consumer group 對同一 event_id 只處理一次：
--   INSERT INTO processed_events VALUES ($event_id, $consumer) ON CONFLICT DO NOTHING
--   → 若 0 列受影響則略過該事件。與業務寫入同一交易。
-- -----------------------------------------------------------------------------
CREATE TABLE processed_events (
    event_id     uuid        NOT NULL,
    consumer     text        NOT NULL,       -- 例：merchant.payment-projector
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);

-- 清理工作依 processed_at 刪除 30 天前紀錄（事件不會重送超過 Kafka retention）
CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

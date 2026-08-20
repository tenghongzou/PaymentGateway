-- =============================================================================
-- webhook-service :: 0002_outbox  (database: pg_webhook)
-- webhook-service 主要是消費者；outbox 用於發佈 webhook.delivery.dead_lettered 等
-- 營運事件（告警、商戶後台通知）。
-- =============================================================================

CREATE TABLE outbox (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type text        NOT NULL,                  -- webhook_delivery
    aggregate_id   text        NOT NULL,
    event_type     text        NOT NULL,                  -- webhook.delivery.dead_lettered / webhook.delivery.canceled / webhook.endpoint.unhealthy
    payload        bytea       NOT NULL,
    headers        jsonb       NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,
    attempts       integer     NOT NULL DEFAULT 0,
    last_error     text
);

CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
CREATE INDEX outbox_published_idx   ON outbox (published_at) WHERE published_at IS NOT NULL;

-- 消費端去重（payment.events / refund.events / merchant.events 各一個 consumer 名稱）。
-- webhook_events.event_id PK 也能擋重複，但 processed_events 涵蓋「被過濾掉、沒有
-- 產生 webhook_events 的事件」，讓 consumer 邏輯一致。
CREATE TABLE processed_events (
    event_id     uuid        NOT NULL,
    consumer     text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);

CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

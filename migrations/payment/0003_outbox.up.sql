-- =============================================================================
-- payment-service :: 0003_outbox  (database: pg_payment)
-- Transactional Outbox + 消費端去重表（docs/01-architecture.md §6.2）
-- topics: payment.events（key = payment_id）、refund.events（key = payment_id，
--         與付款同分割保序）
-- =============================================================================

CREATE TABLE outbox (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),  -- = event_id（消費端去重用）
    aggregate_type text        NOT NULL,                               -- payment / refund / dispute
    aggregate_id   text        NOT NULL,                               -- Kafka key；refund/dispute 也用 payment_id 以保序
    event_type     text        NOT NULL,                               -- payment.captured, refund.succeeded ...
    payload        bytea       NOT NULL,                               -- protobuf pg.payment.v1.PaymentEvent
    headers        jsonb       NOT NULL DEFAULT '{}',                  -- traceparent, schema_version, merchant_id
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,
    attempts       integer     NOT NULL DEFAULT 0,
    last_error     text
);

CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
CREATE INDEX outbox_published_idx   ON outbox (published_at) WHERE published_at IS NOT NULL;

-- payment-service 也是消費者：消費 PSP 正規化事件（provider adapters 經 gateway 轉入）
-- 與 reconciliation.events（差異處理結果）
CREATE TABLE processed_events (
    event_id     uuid        NOT NULL,
    consumer     text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);

CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

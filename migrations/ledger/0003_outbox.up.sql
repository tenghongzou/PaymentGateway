-- =============================================================================
-- ledger-service :: 0003_outbox  (database: pg_ledger)
-- ledger-service 主要是消費者（payment.events / refund.events），但也發佈
-- ledger.events（journal.posted、balance.updated）給 reconciliation / 報表使用。
-- =============================================================================

CREATE TABLE outbox (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type text        NOT NULL,                  -- journal / account
    aggregate_id   text        NOT NULL,                  -- Kafka key（journal 用 merchant_id 以利商戶維度保序）
    event_type     text        NOT NULL,                  -- journal.posted / account.created ...
    payload        bytea       NOT NULL,                  -- protobuf pg.ledger.v1.LedgerEvent
    headers        jsonb       NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,
    attempts       integer     NOT NULL DEFAULT 0,
    last_error     text
);

CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
CREATE INDEX outbox_published_idx   ON outbox (published_at) WHERE published_at IS NOT NULL;

-- 消費端去重。注意：journals.event_id UNIQUE 已是第二道冪等防線，
-- processed_events 則涵蓋「不產生 journal 的事件」（例如 payment.created 只建帳戶）。
CREATE TABLE processed_events (
    event_id     uuid        NOT NULL,
    consumer     text        NOT NULL,                   -- ledger.payment-consumer / ledger.refund-consumer
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);

CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

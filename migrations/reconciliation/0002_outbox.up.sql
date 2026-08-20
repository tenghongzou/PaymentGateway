-- =============================================================================
-- reconciliation-service :: 0002_outbox  (database: pg_recon)
-- 發佈 reconciliation.events（run.completed、discrepancy.opened/resolved）；
-- 消費 payment.events / refund.events / ledger.events 建立讀模型。
-- =============================================================================

CREATE TABLE outbox (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type text        NOT NULL,                  -- reconciliation_run / discrepancy
    aggregate_id   text        NOT NULL,
    event_type     text        NOT NULL,
    payload        bytea       NOT NULL,                  -- protobuf pg.reconciliation.v1.ReconciliationEvent
    headers        jsonb       NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,
    attempts       integer     NOT NULL DEFAULT 0,
    last_error     text
);

CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
CREATE INDEX outbox_published_idx   ON outbox (published_at) WHERE published_at IS NOT NULL;

CREATE TABLE processed_events (
    event_id     uuid        NOT NULL,
    consumer     text        NOT NULL,                    -- recon.payment-projector / recon.ledger-projector
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);

CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

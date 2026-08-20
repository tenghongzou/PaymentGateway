-- =============================================================================
-- payment-service :: 0002_events  (database: pg_payment)
--
-- payment_events：append-only 事件表（Event Sourcing-lite：狀態表 + 事件表）。
-- 每次狀態轉移在同一交易寫入一列，並透過 outbox 發佈 pg.payment.v1.PaymentEvent。
--
-- 分割策略：PARTITION BY RANGE (created_at)，按月（UTC）。
--   * 每筆付款平均 3~5 個事件 → 本表是 payment DB 最大的表。
--   * 分割鍵必須包含在 PK / UNIQUE 中，所以 PK = (id, created_at)、
--     UNIQUE = (payment_id, seq, created_at)。跨分割的 (payment_id, seq) 唯一性由
--     應用層保證：seq 來自 payments.version（樂觀鎖），同一 version 只會成功寫一次。
--   * 分割建立：預先建立「下個月」分割（CronJob 每日呼叫 ensure_monthly_partition），
--     預設分割 payment_events_default 只做保險，監控其列數應恆為 0。
--   * 歸檔：> 13 個月的分割 DETACH PARTITION CONCURRENTLY → pg_dump 到物件儲存 → DROP。
--     保存期限：線上 13 個月、冷儲存 7 年（稽核要求）。
-- =============================================================================

CREATE TABLE payment_events (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    payment_id  uuid        NOT NULL REFERENCES payments (id),
    merchant_id uuid        NOT NULL,
    seq         integer     NOT NULL CHECK (seq >= 1),     -- 同一 payment 內嚴格遞增（= 轉移後的 payments.version）
    event_type  text        NOT NULL,                      -- payment.created / payment.authorized / payment.captured / refund.succeeded ...
    from_status text,                                      -- NULL 代表建立事件
    to_status   text        NOT NULL,
    payload     jsonb       NOT NULL DEFAULT '{}',         -- 事件附帶資料（金額、provider、failure code…）
    actor       text        NOT NULL DEFAULT 'system',     -- system / api_key:<prefix> / psp:<provider> / ops:<user>
    trace_id    text,                                      -- OpenTelemetry trace id，方便追查
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at),
    CONSTRAINT payment_events_payment_seq_key UNIQUE (payment_id, seq, created_at)
) PARTITION BY RANGE (created_at);

-- 重播某筆付款的事件（依 seq）
CREATE INDEX payment_events_payment_seq_idx    ON payment_events (payment_id, seq);
-- 商戶事件時間軸 / 稽核查詢
CREATE INDEX payment_events_merchant_created_idx ON payment_events (merchant_id, created_at DESC);
-- 依事件類型統計
CREATE INDEX payment_events_type_created_idx   ON payment_events (event_type, created_at DESC);

-- append-only：禁止 UPDATE / DELETE（partitioned table 上的 row trigger 會自動複製到各分割）
CREATE TRIGGER payment_events_append_only
    BEFORE UPDATE OR DELETE ON payment_events
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- 預設分割（保險用，監控列數應為 0）
CREATE TABLE payment_events_default PARTITION OF payment_events DEFAULT;

-- 初始月分割（UTC 邊界）
CREATE TABLE payment_events_2026_08 PARTITION OF payment_events
    FOR VALUES FROM ('2026-08-01 00:00:00+00') TO ('2026-09-01 00:00:00+00');
CREATE TABLE payment_events_2026_09 PARTITION OF payment_events
    FOR VALUES FROM ('2026-09-01 00:00:00+00') TO ('2026-10-01 00:00:00+00');

-- -----------------------------------------------------------------------------
-- ensure_monthly_partition(parent, month) — 幂等建立某月分割。
-- 由 CronJob 每日執行：
--   SELECT ensure_monthly_partition('payment_events', date_trunc('month', now() + interval '1 month')::date);
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ensure_monthly_partition(p_parent text, p_month date)
RETURNS text
LANGUAGE plpgsql AS $$
DECLARE
    v_start     date := date_trunc('month', p_month)::date;
    v_end       date := (v_start + interval '1 month')::date;
    v_partition text := format('%s_%s', p_parent, to_char(v_start, 'YYYY_MM'));
BEGIN
    IF to_regclass(v_partition) IS NULL THEN
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
            v_partition, p_parent,
            (v_start::timestamp AT TIME ZONE 'UTC'),
            (v_end::timestamp   AT TIME ZONE 'UTC'));
    END IF;
    RETURN v_partition;
END;
$$;

COMMENT ON TABLE payment_events IS 'append-only 付款事件表（按月分割）；UPDATE/DELETE 由 trigger 拒絕，歸檔以 DETACH PARTITION 進行';

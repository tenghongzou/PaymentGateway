-- =============================================================================
-- webhook-service :: 0001_schema  (database: pg_webhook)
--
-- 職責：消費 payment.events / refund.events / ... → 針對商戶每個啟用的端點產生
-- 一筆 delivery → HMAC-SHA256 簽章送出 → 指數退避重試 → 死信。
--
-- 表：
--   endpoints           — merchant-service webhook_endpoints 的「讀模型」（由 merchant.events 投影）
--   webhook_events      — 已接收並要對商戶通知的事件（event_id = 來源事件 id）
--   webhook_deliveries  — 每 (event, endpoint) 一列的投遞狀態機
--   webhook_delivery_attempts — 每次 HTTP 嘗試的紀錄（商戶後台除錯用）
-- =============================================================================

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- -----------------------------------------------------------------------------
-- endpoints — 讀模型（事實來源：pg_merchant.webhook_endpoints）。
-- 由 merchant.events（webhook_endpoint.created/updated/deleted/secret_rotated）投影；
-- source_version 用來丟棄亂序的舊事件。秘密以 ciphertext 存放（與來源相同的
-- envelope encryption），webhook-service 於簽章時解密。
-- 若投影落後，fan-out 時找不到端點 → 事件先記錄到 webhook_events，由補償工作重新 fan-out。
-- -----------------------------------------------------------------------------
CREATE TABLE endpoints (
    id                uuid        PRIMARY KEY,              -- = merchant-service 的 webhook_endpoints.id
    merchant_id       uuid        NOT NULL,
    url               text        NOT NULL,
    secret_current    text        NOT NULL,                 -- ciphertext
    secret_previous   text,
    secret_rotated_at timestamptz,
    enabled_events    text[]      NOT NULL DEFAULT '{*}',
    status            text        NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled', 'deleted')),
    source_version    integer     NOT NULL DEFAULT 0,       -- 來源 aggregate 的 version
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- fan-out：找出商戶所有啟用端點
CREATE INDEX endpoints_merchant_enabled_idx ON endpoints (merchant_id) WHERE status = 'enabled';

CREATE TRIGGER endpoints_set_updated_at
    BEFORE UPDATE ON endpoints
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- -----------------------------------------------------------------------------
-- webhook_events — 要通知商戶的事件本體（一次寫入；payload 為對外 JSON 格式，
-- 已由 protobuf 轉為 OpenAPI 定義的 event 物件）。
-- 預期量：≈ 所有 payment/refund 狀態轉移數（與 payment_events 同量級）。
-- 分割策略（v1 不分割，說明）：保留 90 天供商戶後台查詢/重送；按月
-- PARTITION BY RANGE (occurred_at) 可行（PK 改為 (event_id, occurred_at)），
-- 但 webhook_deliveries 的 FK 需一併調整；v1 以排程 DELETE（batch 5k 列）清理。
-- -----------------------------------------------------------------------------
CREATE TABLE webhook_events (
    event_id      uuid        PRIMARY KEY,                 -- 來源事件 id（outbox.id），天然去重
    merchant_id   uuid        NOT NULL,
    event_type    text        NOT NULL,                    -- payment.captured / refund.succeeded ...
    resource_type text        NOT NULL,                    -- payment / refund / dispute
    resource_id   text        NOT NULL,                    -- pay_xxx / re_xxx / dp_xxx
    payload       jsonb       NOT NULL,                    -- 對外 JSON（已脫敏）
    occurred_at   timestamptz NOT NULL,                    -- 來源事件發生時間
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- 商戶後台：事件列表 / 依資源查詢
CREATE INDEX webhook_events_merchant_occurred_idx ON webhook_events (merchant_id, occurred_at DESC);
CREATE INDEX webhook_events_resource_idx          ON webhook_events (merchant_id, resource_id);
-- 清理工作
CREATE INDEX webhook_events_occurred_idx          ON webhook_events (occurred_at);

-- -----------------------------------------------------------------------------
-- webhook_deliveries — (event, endpoint) 投遞狀態機（canonical 六個狀態）：
--   pending      初始 / 等待重試
--   in_flight    dispatcher 已取件、HTTP 請求進行中（取件時由 pending|failed 轉入；
--                worker 崩潰時由 reaper 把 in_flight 且 updated_at < now() - timeout 的列轉回 failed）
--   succeeded    收到 2xx（終態）
--   failed       上一次嘗試失敗、已排定 next_attempt_at 等待重試（worker 取件時視同 pending）
--   dead_letter  attempt_no ≥ max（預設 8 次 ≈ 24h）放棄；可由後台手動重設為 pending（終態）
--   canceled     端點被刪除 / 停用時取消尚未成功的投遞（終態）
--
--   pending ──取件──▶ in_flight ──2xx──▶ succeeded
--      ▲                  │ 非 2xx / timeout：attempt_no+1、next_attempt_at = now() + backoff(attempt_no)
--      │                  ▼
--      └──取件──────── failed ──attempt_no ≥ max──▶ dead_letter
--   pending | failed | in_flight ──端點停用/刪除──▶ canceled
--
-- 取件查詢（worker，多副本）：
--   UPDATE webhook_deliveries SET status = 'in_flight', version = version + 1
--    WHERE id IN (SELECT id FROM webhook_deliveries
--                  WHERE status IN ('pending','failed') AND next_attempt_at <= now()
--                  ORDER BY next_attempt_at
--                  FOR UPDATE SKIP LOCKED LIMIT 100)
--   RETURNING *;
--
-- 分割策略（v1 不分割，說明）：量 = webhook_events × 端點數，是 webhook DB 最大表。
-- 按月 PARTITION BY RANGE (created_at) 需把 UNIQUE (event_id, endpoint_id) 改為含
-- created_at；由於 event_id 本身已全域唯一，這個弱化可接受；Phase 1 評估導入。
-- 保留 90 天（與 webhook_events 同步清理）。
-- -----------------------------------------------------------------------------
CREATE TABLE webhook_deliveries (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id             uuid        NOT NULL REFERENCES webhook_events (event_id),
    endpoint_id          uuid        NOT NULL,                  -- 對應 endpoints.id（不建 FK：讀模型可重建）
    merchant_id          uuid        NOT NULL,
    attempt_no           integer     NOT NULL DEFAULT 0 CHECK (attempt_no >= 0),
    status               text        NOT NULL DEFAULT 'pending'
                                     CHECK (status IN ('pending', 'in_flight', 'succeeded', 'failed', 'dead_letter', 'canceled')),
    next_attempt_at      timestamptz NOT NULL DEFAULT now(),
    last_attempt_at      timestamptz,
    last_response_status integer,
    last_response_body   text,                                  -- 截斷至 4KB（應用層負責）
    last_error           text,
    delivered_at         timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    version              integer     NOT NULL DEFAULT 0,
    CONSTRAINT webhook_deliveries_event_endpoint_key UNIQUE (event_id, endpoint_id),
    CONSTRAINT webhook_deliveries_body_len CHECK (last_response_body IS NULL OR length(last_response_body) <= 4096)
);

-- 「撈出待送」查詢：partial index 只含 pending / failed（in_flight 不在內，取件後即離開索引）
CREATE INDEX webhook_deliveries_due_idx
    ON webhook_deliveries (next_attempt_at)
    WHERE status IN ('pending', 'failed');
-- reaper：找出卡住的 in_flight（worker 崩潰）
CREATE INDEX webhook_deliveries_in_flight_idx
    ON webhook_deliveries (updated_at)
    WHERE status = 'in_flight';
-- 商戶後台：依端點看投遞歷史 / 死信清單
CREATE INDEX webhook_deliveries_endpoint_created_idx
    ON webhook_deliveries (endpoint_id, created_at DESC);
CREATE INDEX webhook_deliveries_merchant_status_idx
    ON webhook_deliveries (merchant_id, status, created_at DESC);

CREATE TRIGGER webhook_deliveries_set_updated_at
    BEFORE UPDATE ON webhook_deliveries
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- -----------------------------------------------------------------------------
-- webhook_delivery_attempts — 每次 HTTP 嘗試一列（append-only），商戶後台顯示
-- 「第 n 次：HTTP 503, 1200ms」。保留 30 天。
-- -----------------------------------------------------------------------------
CREATE TABLE webhook_delivery_attempts (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    delivery_id     uuid        NOT NULL REFERENCES webhook_deliveries (id),
    attempt_no      integer     NOT NULL CHECK (attempt_no >= 1),
    response_status integer,
    response_body   text        CHECK (response_body IS NULL OR length(response_body) <= 4096),
    error           text,
    duration_ms     integer     CHECK (duration_ms IS NULL OR duration_ms >= 0),
    attempted_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT webhook_delivery_attempts_delivery_no_key UNIQUE (delivery_id, attempt_no)
);

CREATE INDEX webhook_delivery_attempts_attempted_idx ON webhook_delivery_attempts (attempted_at);

-- =============================================================================
-- payment-service :: 0001_schema  (database: pg_payment)
--
-- 聚合根：payments（含 refunds / disputes 子實體）、payment_attempts（每次呼叫
-- provider 的紀錄）。payment_events（append-only 事件表）在 0002_events。
--
-- 金額一律 bigint 最小貨幣單位（pkg/money）；絕不使用 numeric/float。
-- 本服務不接觸 PAN：payment_method_details 僅允許 last4 / brand / exp_month /
-- exp_year / fingerprint / wallet 等非敏感欄位，由應用層白名單 + 下方 CHECK 雙重把關。
-- =============================================================================

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- 通用「禁止修改/刪除」trigger（給 append-only 表使用）
CREATE OR REPLACE FUNCTION reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: table is append-only', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation',
              HINT    = 'Insert a compensating row instead of mutating history.';
END;
$$;

-- -----------------------------------------------------------------------------
-- payments — Payment 聚合根 / 狀態表
--
-- 分割策略（v1 暫不分割，僅說明）：
--   payments 預期 500 TPS → 每月 ~13 億列上限（實際依 TPS，初期每月數百萬列）。
--   理想上 PARTITION BY RANGE (created_at) 按月分割，但：
--     1. UNIQUE (merchant_id, idempotency_key) 與 UNIQUE (public_id) 必須包含分割鍵，
--        會弱化「跨月」唯一性（idempotency key 只需 24h 唯一，勉強可接受；public_id
--        則需應用層以 UUIDv7 保證）。
--     2. refunds / disputes / payment_attempts 的 FK 需改指向 (id, created_at)。
--   因此 v1 以單表 + B-tree 索引運作，預估 1 億列內效能無虞；超過後改以
--   「新建分割表 + 雙寫 + 背景搬移 + 切換」的零停機方式遷移（見 04-data-model.md §5）。
--   歷史資料（> 18 個月且為終態）由歸檔工作搬到冷儲存。
-- -----------------------------------------------------------------------------
CREATE TABLE payments (
    id                      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id               text        NOT NULL,                               -- pay_xxx
    merchant_id             uuid        NOT NULL,                               -- 跨庫不建 FK
    idempotency_key         text        NOT NULL,                               -- 商戶範圍唯一（最後防線）
    idempotency_request_hash text,                                              -- 同 key 不同 payload → 409（服務層防線）
    amount                  bigint      NOT NULL CHECK (amount >= 0),           -- 請求金額
    currency                char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    capture_method          text        NOT NULL DEFAULT 'automatic'
                                        CHECK (capture_method IN ('automatic', 'manual')),
    status                  text        NOT NULL DEFAULT 'created'
                                        CHECK (status IN (
                                            'created', 'requires_action', 'authorized', 'captured',
                                            'partially_refunded', 'refunded', 'voided', 'failed', 'expired',
                                            'disputed', 'chargeback_won', 'chargeback_lost')),
    -- 終態：captured(→refund 系列) / voided / failed / expired / chargeback_won / chargeback_lost
    --   expired：created / requires_action 在 expires_at 前未完成（獨立終態，與 PSP 拒絕的 failed 區分）
    --   voided ：authorized 被取消；原因見 void_reason（authorized 逾期 → voided + authorization_expired）
    amount_authorized       bigint      NOT NULL DEFAULT 0 CHECK (amount_authorized >= 0),
    amount_captured         bigint      NOT NULL DEFAULT 0 CHECK (amount_captured >= 0),
    amount_refunded         bigint      NOT NULL DEFAULT 0 CHECK (amount_refunded >= 0),
    amount_refund_pending   bigint      NOT NULL DEFAULT 0 CHECK (amount_refund_pending >= 0), -- 進行中退款的保留額
    payment_method_type     text        NOT NULL,                               -- card / wallet / bank_transfer / ...
    payment_method_details  jsonb       NOT NULL DEFAULT '{}',                  -- 僅 last4/brand/exp，絕不含 PAN/CVV
    customer                jsonb       NOT NULL DEFAULT '{}',                  -- {id, email, name, ip}（PII 最小化）
    description             text,
    statement_descriptor    text        CHECK (statement_descriptor IS NULL OR length(statement_descriptor) <= 22),
    return_url              text,                                               -- 3DS / redirect 完成後回跳
    selected_provider       text,                                               -- 最終使用的 PSP（stripe / adyen / mock）
    provider_reference      text,                                               -- PSP 端交易 ID
    failure_category        text,                                               -- declined / fraud / provider_error / timeout / invalid
    failure_code            text,                                               -- 正規化錯誤碼（card_declined, insufficient_funds…）
    failure_message         text,
    expires_at              timestamptz,                                        -- created / requires_action 的完成期限（= flows 文件的 action_expires_at）；逾時 → expired
    auth_expires_at         timestamptz,                                        -- PSP 授權有效期限；逾期前 1h 自動 void → voided (authorization_expired)
    void_reason             text        CHECK (void_reason IS NULL OR void_reason IN
                                            ('merchant_request', 'authorization_expired', 'capture_failed_cleanup', 'risk_decline')),
    authorized_at           timestamptz,
    captured_at             timestamptz,
    voided_at               timestamptz,
    metadata                jsonb       NOT NULL DEFAULT '{}',
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    version                 integer     NOT NULL DEFAULT 0,                     -- 樂觀鎖：每次狀態轉移 +1

    CONSTRAINT payments_public_id_key        UNIQUE (public_id),
    CONSTRAINT payments_public_id_format     CHECK (public_id ~ '^pay_[A-Za-z0-9]+$'),
    -- 冪等最後防線（docs/01-architecture.md §6.1）
    CONSTRAINT payments_merchant_idem_key    UNIQUE (merchant_id, idempotency_key),
    -- 防止超額：授權 ≤ 請求、請款 ≤ 授權、(已退款 + 進行中退款) ≤ 請款
    -- 應用層以樂觀鎖更新：UPDATE payments SET amount_refund_pending = amount_refund_pending + $x, version = version + 1
    --   WHERE id = $id AND version = $v；超額時由下列 CHECK 直接擋下（check_violation → 400 amount_too_large）
    CONSTRAINT payments_authorized_le_amount CHECK (amount_authorized <= amount),
    CONSTRAINT payments_captured_le_auth     CHECK (amount_captured  <= amount_authorized),
    CONSTRAINT payments_refunded_le_captured CHECK (amount_refunded + amount_refund_pending <= amount_captured),
    -- void_reason 只在 voided 狀態有意義
    CONSTRAINT payments_void_reason_state    CHECK (status = 'voided' OR void_reason IS NULL),
    -- PAN 防呆：details 不得含任何看起來像卡號/CVC 的 key
    CONSTRAINT payments_no_pan CHECK (
        NOT (payment_method_details ?| ARRAY['number', 'pan', 'card_number', 'cvc', 'cvv', 'cvv2', 'track1', 'track2'])
    )
);

-- 商戶列表 / 分頁（cursor = created_at, id）
CREATE INDEX payments_merchant_created_idx
    ON payments (merchant_id, created_at DESC, id DESC);
-- 商戶依狀態篩選
CREATE INDEX payments_merchant_status_created_idx
    ON payments (merchant_id, status, created_at DESC);
-- PSP webhook 回查（provider + 其交易 ID）
CREATE INDEX payments_provider_ref_idx
    ON payments (selected_provider, provider_reference)
    WHERE provider_reference IS NOT NULL;
-- expire sweeper：created / requires_action 且 expires_at 已過 → expired
--   （authorized 不在此索引：授權逾期走 auth_expires_at → voided）
CREATE INDEX payments_expires_idx
    ON payments (expires_at)
    WHERE status IN ('created', 'requires_action') AND expires_at IS NOT NULL;
-- void sweeper：authorized 且 auth_expires_at - 1h 已過 → Void → voided (void_reason = authorization_expired)
CREATE INDEX payments_auth_expires_idx
    ON payments (auth_expires_at)
    WHERE status = 'authorized' AND auth_expires_at IS NOT NULL;
-- 對帳 / 報表：依 captured_at 掃描
CREATE INDEX payments_captured_at_idx
    ON payments (captured_at)
    WHERE captured_at IS NOT NULL;

CREATE TRIGGER payments_set_updated_at
    BEFORE UPDATE ON payments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE  payments IS 'Payment 聚合根；狀態轉移一律經 domain 狀態機並寫 payment_events + outbox';
COMMENT ON COLUMN payments.version IS '樂觀鎖：UPDATE payments SET ..., version = version + 1 WHERE id = $1 AND version = $2';
COMMENT ON COLUMN payments.payment_method_details IS '只允許非敏感欄位（last4, brand, exp_month, exp_year, funding, wallet, fingerprint）';

-- -----------------------------------------------------------------------------
-- payment_attempts — 每次對 provider 的呼叫（authorize / capture / void），
-- 支援 failover 追蹤與 provider 健康度統計。
-- 預期量：payments × 1.1 ~ 1.5；與 payments 相同生命週期，一併歸檔。
-- -----------------------------------------------------------------------------
CREATE TABLE payment_attempts (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id         uuid        NOT NULL REFERENCES payments (id),
    merchant_id        uuid        NOT NULL,
    attempt_no         integer     NOT NULL CHECK (attempt_no >= 1),
    operation          text        NOT NULL DEFAULT 'authorize'
                                   CHECK (operation IN ('authorize', 'capture', 'void', 'status_sync')),
    provider           text        NOT NULL,
    provider_reference text,
    -- approved：成功；declined：PSP 明確拒絕（不 failover）；unavailable：PSP 故障/限流（可 failover）；
    -- unknown：逾時、結果不明（需 GetPaymentStatus 查證，不可直接 failover）
    status             text        NOT NULL DEFAULT 'pending'
                                   CHECK (status IN ('pending', 'approved', 'requires_action', 'declined', 'unavailable', 'unknown')),
    error_category     text,                                   -- retryable / non_retryable / timeout
    error_code         text,
    error_message      text,
    request_snapshot   jsonb       NOT NULL DEFAULT '{}',      -- 已遮罩；不含 PAN、不含 PSP secret
    response_snapshot  jsonb       NOT NULL DEFAULT '{}',      -- 已遮罩
    latency_ms         integer     CHECK (latency_ms IS NULL OR latency_ms >= 0),
    created_at         timestamptz NOT NULL DEFAULT now(),
    completed_at       timestamptz,
    CONSTRAINT payment_attempts_payment_no_key UNIQUE (payment_id, attempt_no)
);

-- provider 健康度 / 延遲統計
CREATE INDEX payment_attempts_provider_created_idx
    ON payment_attempts (provider, created_at DESC);

-- -----------------------------------------------------------------------------
-- refunds — 退款（docs/01-architecture.md §5.3：pending → succeeded | failed）
-- -----------------------------------------------------------------------------
CREATE TABLE refunds (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id          text        NOT NULL,                     -- re_xxx
    payment_id         uuid        NOT NULL REFERENCES payments (id),
    merchant_id        uuid        NOT NULL,
    idempotency_key    text        NOT NULL,
    amount             bigint      NOT NULL CHECK (amount > 0),
    currency           char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    status             text        NOT NULL DEFAULT 'pending'
                                   CHECK (status IN ('pending', 'succeeded', 'failed')),
    reason             text        CHECK (reason IS NULL OR reason IN
                                   ('requested_by_customer', 'duplicate', 'fraudulent', 'other')),
    provider           text,
    provider_reference text,
    failure_code       text,
    failure_message    text,
    metadata           jsonb       NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    succeeded_at       timestamptz,
    version            integer     NOT NULL DEFAULT 0,
    CONSTRAINT refunds_public_id_key     UNIQUE (public_id),
    CONSTRAINT refunds_public_id_format  CHECK (public_id ~ '^re_[A-Za-z0-9]+$'),
    CONSTRAINT refunds_merchant_idem_key UNIQUE (merchant_id, idempotency_key)
);

CREATE INDEX refunds_payment_idx          ON refunds (payment_id);
CREATE INDEX refunds_merchant_created_idx ON refunds (merchant_id, created_at DESC, id DESC);
CREATE INDEX refunds_provider_ref_idx     ON refunds (provider, provider_reference)
    WHERE provider_reference IS NOT NULL;

CREATE TRIGGER refunds_set_updated_at
    BEFORE UPDATE ON refunds
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 超額退款防線（DB 層 backstop）。
-- 主要保證在應用層：建立退款時於同一交易內以樂觀鎖更新 payments.amount_refunded，
-- 而 payments_refunded_le_captured CHECK 會擋下超額。此 trigger 另外確保「所有
-- 未失敗的退款總額 ≤ amount_captured」，並以 FOR UPDATE 鎖住 payment 列，把同一
-- payment 的併發退款序列化（同一交易內若已持有該列鎖則無額外成本）。
CREATE OR REPLACE FUNCTION refunds_guard_total() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_captured bigint;
    v_currency char(3);
    v_total    bigint;
BEGIN
    SELECT amount_captured, currency
      INTO v_captured, v_currency
      FROM payments
     WHERE id = NEW.payment_id
       FOR UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'refund %: payment % not found', NEW.id, NEW.payment_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;

    IF NEW.currency <> v_currency THEN
        RAISE EXCEPTION 'refund %: currency % does not match payment currency %', NEW.id, NEW.currency, v_currency
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT COALESCE(SUM(amount), 0)
      INTO v_total
      FROM refunds
     WHERE payment_id = NEW.payment_id
       AND status <> 'failed';

    IF v_total > v_captured THEN
        RAISE EXCEPTION 'refund %: total refunds % exceed captured amount % for payment %',
            NEW.id, v_total, v_captured, NEW.payment_id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;

CREATE TRIGGER refunds_guard_total
    AFTER INSERT OR UPDATE OF amount, status ON refunds
    FOR EACH ROW EXECUTE FUNCTION refunds_guard_total();

-- -----------------------------------------------------------------------------
-- disputes — 爭議 / 拒付（由 PSP webhook 建立；付款狀態同步轉 disputed →
-- chargeback_won / chargeback_lost）
-- -----------------------------------------------------------------------------
CREATE TABLE disputes (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id           text        NOT NULL,                    -- dp_xxx
    payment_id          uuid        NOT NULL REFERENCES payments (id),
    merchant_id         uuid        NOT NULL,
    provider            text        NOT NULL,
    provider_dispute_id text        NOT NULL,
    amount              bigint      NOT NULL CHECK (amount >= 0),
    currency            char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    status              text        NOT NULL DEFAULT 'open'
                                    CHECK (status IN ('open', 'evidence_submitted', 'under_review', 'won', 'lost', 'closed')),
    reason              text,                                    -- fraudulent / product_not_received / duplicate / ...
    evidence_due_at     timestamptz,
    evidence            jsonb       NOT NULL DEFAULT '{}',       -- 已提交證據的描述/檔案參照（不存檔案本體）
    evidence_submitted_at timestamptz,
    metadata            jsonb       NOT NULL DEFAULT '{}',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    closed_at           timestamptz,
    version             integer     NOT NULL DEFAULT 0,
    CONSTRAINT disputes_public_id_key        UNIQUE (public_id),
    CONSTRAINT disputes_public_id_format     CHECK (public_id ~ '^dp_[A-Za-z0-9]+$'),
    -- PSP 重送同一 dispute webhook 時冪等
    CONSTRAINT disputes_provider_dispute_key UNIQUE (provider, provider_dispute_id)
);

CREATE INDEX disputes_payment_idx          ON disputes (payment_id);
CREATE INDEX disputes_merchant_created_idx ON disputes (merchant_id, created_at DESC);
-- 證據截止提醒工作
CREATE INDEX disputes_evidence_due_idx     ON disputes (evidence_due_at)
    WHERE status IN ('open') AND evidence_due_at IS NOT NULL;

CREATE TRIGGER disputes_set_updated_at
    BEFORE UPDATE ON disputes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

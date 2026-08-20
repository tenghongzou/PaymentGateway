-- =============================================================================
-- reconciliation-service :: 0001_schema  (database: pg_recon)
--
-- 職責：匯入 PSP 結算檔 → 與內部紀錄（付款/退款、帳本）比對 → 產生差異 → 人工/自動處置。
--
-- 沒有跨庫 JOIN：比對所需的內部資料由事件投影成本地讀模型：
--   payment_records — 來自 payment.events / refund.events（provider_reference、金額、狀態）
--   ledger_postings — 來自 ledger.events（journal.posted：reference、金額）
-- =============================================================================

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- -----------------------------------------------------------------------------
-- settlement_files — 匯入的 PSP 結算檔（CSV / JSON / API 拉取快照）。
-- file_hash UNIQUE：同一檔案重複上傳冪等。
-- -----------------------------------------------------------------------------
CREATE TABLE settlement_files (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider     text        NOT NULL,                          -- stripe / adyen / linepay / ecpay / mock
    file_name    text        NOT NULL,
    file_hash    text        NOT NULL,                          -- sha256 hex
    storage_uri  text,                                          -- 原檔在物件儲存的位置（s3://...）
    period_start date,
    period_end   date,
    row_count    integer     NOT NULL DEFAULT 0 CHECK (row_count >= 0),
    status       text        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'importing', 'imported', 'failed')),
    error        text,
    imported_at  timestamptz,
    metadata     jsonb       NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    version      integer     NOT NULL DEFAULT 0,
    CONSTRAINT settlement_files_hash_key UNIQUE (file_hash)
);

CREATE INDEX settlement_files_provider_created_idx ON settlement_files (provider, created_at DESC);

CREATE TRIGGER settlement_files_set_updated_at
    BEFORE UPDATE ON settlement_files
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- -----------------------------------------------------------------------------
-- settlement_lines — 結算檔每一列正規化後的內容；raw 保留原始欄位以利除錯。
-- 預期量：每日每 PSP 數萬 ~ 數十萬列；保留 25 個月後歸檔（檔案本體仍在物件儲存）。
-- 分割策略：v1 不分割；量大時按 settled_at 月分割（PK 改 (id, settled_at)）。
-- -----------------------------------------------------------------------------
CREATE TABLE settlement_lines (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id            uuid        NOT NULL REFERENCES settlement_files (id),
    line_no            integer     NOT NULL CHECK (line_no >= 1),
    provider           text        NOT NULL,
    provider_reference text        NOT NULL,                    -- PSP 交易 ID（對應 payments.provider_reference）
    merchant_reference text,                                    -- PSP 帶回的我方參照（pay_xxx / re_xxx），若有
    type               text        NOT NULL
                                   CHECK (type IN ('payment', 'refund', 'chargeback', 'fee', 'adjustment')),
    amount             bigint      NOT NULL CHECK (amount >= 0), -- 最小貨幣單位；方向由 type 決定
    currency           char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    settled_at         timestamptz NOT NULL,
    raw                jsonb       NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT settlement_lines_file_line_key UNIQUE (file_id, line_no)
);

-- 比對主鍵：provider + provider_reference（+ type，同一交易可有 payment 與 fee 兩列）
CREATE INDEX settlement_lines_provider_ref_idx ON settlement_lines (provider, provider_reference, type);
CREATE INDEX settlement_lines_settled_idx      ON settlement_lines (provider, settled_at);

-- -----------------------------------------------------------------------------
-- payment_records — 讀模型：付款 / 退款 / 拒付在 PSP 端的參照與金額（由 payment.events 投影）
-- -----------------------------------------------------------------------------
CREATE TABLE payment_records (
    id                 uuid        PRIMARY KEY,                 -- payment-service 的 payments.id / refunds.id / disputes.id
    kind               text        NOT NULL CHECK (kind IN ('payment', 'refund', 'dispute')),
    public_id          text        NOT NULL,                    -- pay_xxx / re_xxx / dp_xxx
    merchant_id        uuid        NOT NULL,
    provider           text,
    provider_reference text,
    amount             bigint      NOT NULL CHECK (amount >= 0), -- payment: amount_captured；refund: amount
    currency           char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    status             text        NOT NULL,
    occurred_at        timestamptz NOT NULL,                    -- captured_at / succeeded_at
    source_seq         integer     NOT NULL DEFAULT 0,          -- 來源事件 seq，丟棄亂序舊事件
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payment_records_public_id_key UNIQUE (public_id)
);

CREATE INDEX payment_records_provider_ref_idx ON payment_records (provider, provider_reference)
    WHERE provider_reference IS NOT NULL;
CREATE INDEX payment_records_merchant_occurred_idx ON payment_records (merchant_id, occurred_at DESC);

CREATE TRIGGER payment_records_set_updated_at
    BEFORE UPDATE ON payment_records
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- -----------------------------------------------------------------------------
-- ledger_postings — 讀模型：已入帳 journal 的摘要（由 ledger.events journal.posted 投影）
-- -----------------------------------------------------------------------------
CREATE TABLE ledger_postings (
    journal_id     uuid        PRIMARY KEY,
    merchant_id    uuid        NOT NULL,
    reference_type text        NOT NULL,
    reference_id   text        NOT NULL,                        -- pay_xxx / re_xxx ...
    amount         bigint      NOT NULL CHECK (amount >= 0),    -- journal 借方合計
    currency       char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    posted_at      timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ledger_postings_reference_idx ON ledger_postings (reference_type, reference_id);
CREATE INDEX ledger_postings_merchant_posted_idx ON ledger_postings (merchant_id, posted_at DESC);

-- -----------------------------------------------------------------------------
-- reconciliation_runs — 一次對帳執行（provider × 期間）
-- -----------------------------------------------------------------------------
CREATE TABLE reconciliation_runs (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id       text        NOT NULL,                       -- rr_xxx
    provider        text        NOT NULL,
    period_start    timestamptz NOT NULL,
    period_end      timestamptz NOT NULL,
    status          text        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    matched_count   integer     NOT NULL DEFAULT 0 CHECK (matched_count >= 0),
    unmatched_count integer     NOT NULL DEFAULT 0 CHECK (unmatched_count >= 0),
    summary         jsonb       NOT NULL DEFAULT '{}',          -- 各 kind 計數、金額合計、耗時…
    error           text,
    started_at      timestamptz,
    finished_at     timestamptz,
    triggered_by    text        NOT NULL DEFAULT 'scheduler',   -- scheduler / ops:<user>
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    version         integer     NOT NULL DEFAULT 0,
    CONSTRAINT reconciliation_runs_public_id_key UNIQUE (public_id),
    CONSTRAINT reconciliation_runs_period CHECK (period_end > period_start)
);

CREATE INDEX reconciliation_runs_provider_period_idx ON reconciliation_runs (provider, period_start DESC);

CREATE TRIGGER reconciliation_runs_set_updated_at
    BEFORE UPDATE ON reconciliation_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- -----------------------------------------------------------------------------
-- discrepancies — 差異項目與處置
-- -----------------------------------------------------------------------------
CREATE TABLE discrepancies (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id             uuid        NOT NULL REFERENCES reconciliation_runs (id),
    merchant_id        uuid,                                     -- missing_in_ledger 且無法對應商戶時為 NULL
    kind               text        NOT NULL
                                   CHECK (kind IN ('missing_in_ledger', 'missing_in_psp', 'amount_mismatch', 'status_mismatch')),
    provider           text        NOT NULL,
    provider_reference text,
    internal_reference text,                                    -- pay_xxx / re_xxx / jrn_xxx
    settlement_line_id uuid        REFERENCES settlement_lines (id),
    expected_amount    bigint      CHECK (expected_amount IS NULL OR expected_amount >= 0),
    actual_amount      bigint      CHECK (actual_amount   IS NULL OR actual_amount   >= 0),
    currency           char(3)     CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    status             text        NOT NULL DEFAULT 'open'
                                   CHECK (status IN ('open', 'resolved', 'ignored')),
    resolution_note    text,
    resolved_by        text,                                    -- ops:<user> / auto:<rule>
    resolved_at        timestamptz,
    details            jsonb       NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    version            integer     NOT NULL DEFAULT 0,
    CONSTRAINT discrepancies_resolved_consistency CHECK (
        (status = 'open' AND resolved_at IS NULL) OR
        (status IN ('resolved', 'ignored') AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX discrepancies_run_idx            ON discrepancies (run_id, kind);
-- 待處理清單（營運後台）
CREATE INDEX discrepancies_open_idx           ON discrepancies (created_at) WHERE status = 'open';
CREATE INDEX discrepancies_merchant_idx       ON discrepancies (merchant_id, created_at DESC) WHERE merchant_id IS NOT NULL;
-- 同一 provider_reference 是否已有差異（避免跨 run 重複開單）
CREATE INDEX discrepancies_provider_ref_idx   ON discrepancies (provider, provider_reference) WHERE provider_reference IS NOT NULL;

CREATE TRIGGER discrepancies_set_updated_at
    BEFORE UPDATE ON discrepancies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

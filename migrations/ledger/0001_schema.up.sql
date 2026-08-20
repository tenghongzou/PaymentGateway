-- =============================================================================
-- ledger-service :: 0001_schema  (database: pg_ledger)
--
-- 雙式記帳帳本（docs/01-architecture.md §5.4）：
--   * accounts  — 科目/帳戶（系統帳戶 merchant_id IS NULL；商戶帳戶帶 merchant_id）
--   * journals  — 日記帳（一個業務事件一筆；event_id UNIQUE 保證冪等）
--   * entries   — 分錄（按月分割；0002_entries）
--   * balances  — 每個帳戶的即時餘額（由 trigger 維護，見 0002）
--   * balance_snapshots — 每日餘額快照（對帳 / 漂移偵測）
--
-- DB 級不變條件（0002_entries 實作）：
--   I1. 同一 journal 的借方合計 = 貸方合計（CONSTRAINT TRIGGER DEFERRABLE INITIALLY DEFERRED）
--   I2. 同一 journal 至少兩筆分錄、且幣別一致
--   I3. 分錄幣別 = 帳戶幣別；帳戶必須為 active
--   I4. journals / entries 只能 INSERT（trigger 拒絕 UPDATE / DELETE / TRUNCATE）
--       錯帳一律以反向分錄（reversal journal）沖銷
-- =============================================================================

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger: % on % is not allowed (append-only); post a reversing journal instead',
        TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

-- -----------------------------------------------------------------------------
-- accounts
-- code 對應 01-architecture.md §5.4 的帳戶類型：
--   merchant_payable   liability / credit   （應付商戶款；每商戶每幣別一個）
--   psp_receivable     asset     / debit    （應收 PSP 款；系統帳戶，每 provider 每幣別一個）
--   fee_revenue        revenue   / credit   （平台手續費收入；系統帳戶）
--   refund_clearing    liability / credit   （退款在途；每商戶每幣別）
--   chargeback_reserve liability / credit   （拒付準備金；每商戶每幣別）
-- 預期量：商戶數 × 幣別數 × 3 + 系統帳戶；< 1M 列
-- -----------------------------------------------------------------------------
CREATE TABLE accounts (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id    uuid,                                            -- NULL = 系統帳戶
    code           text        NOT NULL,                            -- merchant_payable / psp_receivable:stripe / fee_revenue ...
    name           text        NOT NULL,
    type           text        NOT NULL CHECK (type IN ('asset', 'liability', 'revenue', 'expense')),
    normal_balance text        NOT NULL CHECK (normal_balance IN ('debit', 'credit')),
    currency       char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    status         text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen', 'closed')),
    metadata       jsonb       NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    version        integer     NOT NULL DEFAULT 0,
    -- 會計恆等式：資產/費用為借方科目，負債/收入為貸方科目
    CONSTRAINT accounts_type_normal_balance CHECK (
        (type IN ('asset', 'expense')    AND normal_balance = 'debit') OR
        (type IN ('liability', 'revenue') AND normal_balance = 'credit')
    ),
    -- PG15+：NULLS NOT DISTINCT 讓系統帳戶（merchant_id NULL）也受唯一約束
    CONSTRAINT accounts_merchant_code_currency_key UNIQUE NULLS NOT DISTINCT (merchant_id, code, currency)
);

CREATE INDEX accounts_merchant_idx ON accounts (merchant_id, currency) WHERE merchant_id IS NOT NULL;

CREATE TRIGGER accounts_set_updated_at
    BEFORE UPDATE ON accounts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- -----------------------------------------------------------------------------
-- journals — 日記帳。一個業務事件（payment.captured、refund.succeeded、
-- chargeback、fee、reversal…）對應一筆 journal。
-- event_id UNIQUE：同一事件重複消費時 INSERT 會違反唯一鍵 → 消費者視為已處理。
-- 不可變：無 updated_at / version。
-- 預期量：約等於「終態付款 + 退款 + 拒付 + 手續費 + 調整」數量；
-- v1 不分割（journal 列小、查詢多為 by id / by reference），超過 2 億列再評估按 posted_at 分割。
-- -----------------------------------------------------------------------------
CREATE TABLE journals (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id              text        NOT NULL,                           -- jrn_xxx
    merchant_id            uuid        NOT NULL,
    reference_type         text        NOT NULL
                                       CHECK (reference_type IN ('payment', 'refund', 'dispute', 'fee', 'settlement', 'adjustment', 'reversal')),
    reference_id           text        NOT NULL,                           -- 例：pay_xxx / re_xxx / dp_xxx / settlement file id
    event_id               uuid        NOT NULL,                           -- 來源 Kafka 事件 id（冪等鍵）
    description            text,
    reversal_of_journal_id uuid        REFERENCES journals (id),           -- 沖銷時指向原 journal
    posted_at              timestamptz NOT NULL DEFAULT now(),             -- 會計入帳時間（可等於事件 occurred_at）
    metadata               jsonb       NOT NULL DEFAULT '{}',
    created_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT journals_public_id_key UNIQUE (public_id),
    CONSTRAINT journals_public_id_format CHECK (public_id ~ '^jrn_[A-Za-z0-9]+$'),
    CONSTRAINT journals_event_id_key  UNIQUE (event_id)
);

CREATE INDEX journals_merchant_posted_idx   ON journals (merchant_id, posted_at DESC);
CREATE INDEX journals_reference_idx         ON journals (reference_type, reference_id);
CREATE INDEX journals_reversal_of_idx       ON journals (reversal_of_journal_id) WHERE reversal_of_journal_id IS NOT NULL;

-- I4：append-only
CREATE TRIGGER journals_append_only
    BEFORE UPDATE OR DELETE ON journals
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER journals_no_truncate
    BEFORE TRUNCATE ON journals
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- -----------------------------------------------------------------------------
-- balances — 每帳戶即時餘額（以 normal_balance 方向為正）。
-- 由 0002 的 entries AFTER INSERT trigger 維護；帳戶建立時由 trigger 自動建立一列。
-- 同一帳戶的分錄寫入會在這一列序列化（row lock）；merchant_payable 是熱點，
-- 高 TPS 下可改為「非同步投影」（見 04-data-model.md §6.3）。
-- -----------------------------------------------------------------------------
CREATE TABLE balances (
    account_id     uuid        PRIMARY KEY REFERENCES accounts (id),
    currency       char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance        bigint      NOT NULL DEFAULT 0,               -- 可為負（例如商戶欠款），由業務層決定是否允許
    entry_count    bigint      NOT NULL DEFAULT 0,
    as_of_entry_id uuid,                                         -- 最後一筆納入計算的 entry
    updated_at     timestamptz NOT NULL DEFAULT now(),
    version        bigint      NOT NULL DEFAULT 0
);

CREATE OR REPLACE FUNCTION accounts_create_balance() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO balances (account_id, currency) VALUES (NEW.id, NEW.currency);
    RETURN NULL;
END;
$$;

CREATE TRIGGER accounts_create_balance
    AFTER INSERT ON accounts
    FOR EACH ROW EXECUTE FUNCTION accounts_create_balance();

-- -----------------------------------------------------------------------------
-- balance_snapshots — 每日（UTC 00:00）餘額快照；用於：
--   * 對帳（reconciliation）取某日餘額
--   * 漂移偵測：SUM(entries) 至 snapshot 時間應等於 snapshot.balance
--   * 歸檔舊分錄分割後仍能重建餘額（snapshot + 之後的分錄）
-- -----------------------------------------------------------------------------
CREATE TABLE balance_snapshots (
    account_id     uuid        NOT NULL REFERENCES accounts (id),
    snapshot_date  date        NOT NULL,
    balance        bigint      NOT NULL,
    entry_count    bigint      NOT NULL DEFAULT 0,
    as_of_entry_id uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, snapshot_date)
);

CREATE INDEX balance_snapshots_date_idx ON balance_snapshots (snapshot_date);

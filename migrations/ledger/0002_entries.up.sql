-- =============================================================================
-- ledger-service :: 0002_entries  (database: pg_ledger)
--
-- entries：分錄，append-only，PARTITION BY RANGE (created_at) 按月（UTC）。
--   * 每筆 journal 2~4 筆分錄 → 帳本最大的表。
--   * PK = (id, created_at)（分割鍵必須在 PK 內）。
--   * 歸檔：> 25 個月的分割 DETACH → dump 到冷儲存 → DROP；因 balance_snapshots
--     存在，歸檔後仍可由「最近快照 + 之後分錄」重建任一帳戶餘額。
--     保存期限：線上 25 個月（兩個完整會計年度）、冷儲存 10 年。
-- =============================================================================

CREATE TABLE entries (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    journal_id  uuid        NOT NULL REFERENCES journals (id),
    account_id  uuid        NOT NULL REFERENCES accounts (id),
    merchant_id uuid,                                                -- 反正規化自 accounts（系統帳戶為 NULL），由 trigger 填入
    direction   text        NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount      bigint      NOT NULL CHECK (amount > 0),            -- 分錄金額恆為正；方向由 direction 表示
    currency    char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

-- 取出某筆 journal 的分錄（平衡檢查、明細查詢）
CREATE INDEX entries_journal_idx          ON entries (journal_id);
-- 帳戶明細 / 對帳單（statement）
CREATE INDEX entries_account_created_idx  ON entries (account_id, created_at DESC);
-- 商戶維度查詢
CREATE INDEX entries_merchant_created_idx ON entries (merchant_id, created_at DESC) WHERE merchant_id IS NOT NULL;

CREATE TABLE entries_default PARTITION OF entries DEFAULT;
CREATE TABLE entries_2026_08 PARTITION OF entries
    FOR VALUES FROM ('2026-08-01 00:00:00+00') TO ('2026-09-01 00:00:00+00');
CREATE TABLE entries_2026_09 PARTITION OF entries
    FOR VALUES FROM ('2026-09-01 00:00:00+00') TO ('2026-10-01 00:00:00+00');

-- 幂等建立月分割（CronJob 每日：SELECT ensure_monthly_partition('entries', (date_trunc('month', now()) + interval '1 month')::date)）
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

-- -----------------------------------------------------------------------------
-- I3（BEFORE INSERT，立即）：帳戶存在且 active、幣別一致；填入 merchant_id。
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION entries_before_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_acct accounts%ROWTYPE;
BEGIN
    SELECT * INTO v_acct FROM accounts WHERE id = NEW.account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger: account % not found', NEW.account_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF v_acct.status <> 'active' THEN
        RAISE EXCEPTION 'ledger: account % is % (must be active to post)', NEW.account_id, v_acct.status
            USING ERRCODE = 'check_violation';
    END IF;
    IF v_acct.currency <> NEW.currency THEN
        RAISE EXCEPTION 'ledger: entry currency % does not match account % currency %',
            NEW.currency, NEW.account_id, v_acct.currency
            USING ERRCODE = 'check_violation';
    END IF;
    NEW.merchant_id := v_acct.merchant_id;
    RETURN NEW;
END;
$$;

CREATE TRIGGER entries_before_insert
    BEFORE INSERT ON entries
    FOR EACH ROW EXECUTE FUNCTION entries_before_insert();

-- -----------------------------------------------------------------------------
-- balances 維護（AFTER INSERT，立即）：以 normal_balance 方向為正。
--   借方科目（asset/expense）：debit 增加、credit 減少
--   貸方科目（liability/revenue）：credit 增加、debit 減少
-- UPDATE balances 會取得該帳戶的 row lock，將同帳戶的併發入帳序列化。
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION entries_apply_balance() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_normal text;
    v_delta  bigint;
BEGIN
    SELECT normal_balance INTO v_normal FROM accounts WHERE id = NEW.account_id;
    v_delta := CASE WHEN NEW.direction = v_normal THEN NEW.amount ELSE -NEW.amount END;

    UPDATE balances
       SET balance        = balance + v_delta,
           entry_count    = entry_count + 1,
           as_of_entry_id = NEW.id,
           version        = version + 1,
           updated_at     = now()
     WHERE account_id = NEW.account_id;

    IF NOT FOUND THEN
        -- accounts_create_balance trigger 應已建立；這裡是防禦性補建
        INSERT INTO balances (account_id, currency, balance, entry_count, as_of_entry_id, version)
        VALUES (NEW.account_id, NEW.currency, v_delta, 1, NEW.id, 1);
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER entries_apply_balance
    AFTER INSERT ON entries
    FOR EACH ROW EXECUTE FUNCTION entries_apply_balance();

-- -----------------------------------------------------------------------------
-- I1 + I2（CONSTRAINT TRIGGER，DEFERRABLE INITIALLY DEFERRED）：
-- 在交易 COMMIT 時檢查每筆被觸及的 journal：
--   * 借方合計 = 貸方合計
--   * 至少兩筆分錄
--   * 幣別一致
-- 掛在 journals（擋「只有 journal 沒有分錄」）與 entries（擋「分錄不平衡」）兩邊。
-- 應用層仍須在同一交易內寫入 journal + 全部分錄；deferred 允許逐筆 INSERT 分錄而
-- 不需要一次多值 INSERT。
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION assert_journal_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_journal_id uuid;
    v_debits     bigint;
    v_credits    bigint;
    v_count      integer;
    v_currencies integer;
BEGIN
    IF TG_TABLE_NAME = 'journals' THEN
        v_journal_id := NEW.id;
    ELSE
        v_journal_id := NEW.journal_id;
    END IF;

    SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'),  0),
           COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0),
           COUNT(*),
           COUNT(DISTINCT currency)
      INTO v_debits, v_credits, v_count, v_currencies
      FROM entries
     WHERE journal_id = v_journal_id;

    IF v_count < 2 THEN
        RAISE EXCEPTION 'ledger: journal % must have at least two entries (has %)', v_journal_id, v_count
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v_currencies <> 1 THEN
        RAISE EXCEPTION 'ledger: journal % mixes % currencies', v_journal_id, v_currencies
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v_debits <> v_credits THEN
        RAISE EXCEPTION 'ledger: journal % is unbalanced (debits=% credits=%)', v_journal_id, v_debits, v_credits
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER journals_balanced
    AFTER INSERT ON journals
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_journal_balanced();

CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_journal_balanced();

-- I4：append-only（row trigger 自動複製到各分割，含日後由 ensure_monthly_partition 建立者）
CREATE TRIGGER entries_append_only
    BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- -----------------------------------------------------------------------------
-- v_balance_drift — 餘額漂移偵測 view（pg_ledger_imbalance_total 指標來源，應恆為 0 列）。
-- 全表掃描 entries，僅供排程 / 人工檢查，勿在線上請求路徑使用。
-- -----------------------------------------------------------------------------
CREATE VIEW v_balance_drift AS
SELECT b.account_id,
       b.balance AS recorded_balance,
       COALESCE(SUM(CASE WHEN e.direction = a.normal_balance THEN e.amount ELSE -e.amount END), 0) AS computed_balance
  FROM balances b
  JOIN accounts a ON a.id = b.account_id
  LEFT JOIN entries e ON e.account_id = b.account_id
 GROUP BY b.account_id, b.balance
HAVING b.balance <> COALESCE(SUM(CASE WHEN e.direction = a.normal_balance THEN e.amount ELSE -e.amount END), 0);

-- -----------------------------------------------------------------------------
-- 權限：應用程式角色即使繞過 trigger 也不該有 UPDATE/DELETE 權限（縱深防禦）。
-- 角色由 deploy/compose/postgres/init.sql（本機）或 Vault/Terraform（生產）建立；
-- 不存在時略過，避免 migration 與環境耦合。
-- -----------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_app') THEN
        REVOKE UPDATE, DELETE, TRUNCATE ON journals, entries FROM ledger_app;
    END IF;
END;
$$;

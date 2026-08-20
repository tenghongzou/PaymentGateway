-- ledger-service :: 0002_entries (down)
DROP VIEW IF EXISTS v_balance_drift;
DROP TRIGGER IF EXISTS journals_balanced ON journals;
DROP TABLE IF EXISTS entries;                 -- 連同分割、其上的 triggers
DROP FUNCTION IF EXISTS assert_journal_balanced();
DROP FUNCTION IF EXISTS entries_apply_balance();
DROP FUNCTION IF EXISTS entries_before_insert();
DROP FUNCTION IF EXISTS ensure_monthly_partition(text, date);

-- ledger-service :: 0001_schema (down)
DROP TABLE IF EXISTS balance_snapshots;
DROP TRIGGER IF EXISTS accounts_create_balance ON accounts;
DROP FUNCTION IF EXISTS accounts_create_balance();
DROP TABLE IF EXISTS balances;
DROP TABLE IF EXISTS journals;
DROP TABLE IF EXISTS accounts;
DROP FUNCTION IF EXISTS reject_mutation();
DROP FUNCTION IF EXISTS set_updated_at();

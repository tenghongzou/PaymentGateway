-- reconciliation-service :: 0001_schema (down)
DROP TABLE IF EXISTS discrepancies;
DROP TABLE IF EXISTS reconciliation_runs;
DROP TABLE IF EXISTS ledger_postings;
DROP TABLE IF EXISTS payment_records;
DROP TABLE IF EXISTS settlement_lines;
DROP TABLE IF EXISTS settlement_files;
DROP FUNCTION IF EXISTS set_updated_at();

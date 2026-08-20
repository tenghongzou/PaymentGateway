-- payment-service :: 0001_schema (down)
DROP TABLE IF EXISTS disputes;
DROP TRIGGER IF EXISTS refunds_guard_total ON refunds;
DROP TABLE IF EXISTS refunds;
DROP FUNCTION IF EXISTS refunds_guard_total();
DROP TABLE IF EXISTS payment_attempts;
DROP TABLE IF EXISTS payments;
DROP FUNCTION IF EXISTS reject_mutation();
DROP FUNCTION IF EXISTS set_updated_at();

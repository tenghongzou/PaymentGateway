-- payment-service :: 0002_events (down)
DROP FUNCTION IF EXISTS ensure_monthly_partition(text, date);
-- 刪除父表會一併刪除所有分割
DROP TABLE IF EXISTS payment_events;

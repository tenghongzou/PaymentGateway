-- webhook-service :: 0001_schema (down)
DROP TABLE IF EXISTS webhook_delivery_attempts;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS endpoints;
DROP FUNCTION IF EXISTS set_updated_at();

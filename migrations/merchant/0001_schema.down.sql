-- merchant-service :: 0001_schema (down)
DROP TABLE IF EXISTS routing_preferences;
DROP TABLE IF EXISTS webhook_endpoints;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS merchants;
DROP FUNCTION IF EXISTS set_updated_at();

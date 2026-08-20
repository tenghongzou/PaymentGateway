-- =============================================================================
-- PaymentGateway — 本機開發 PostgreSQL 初始化腳本
-- 掛載位置：/docker-entrypoint-initdb.d/10-init.sql（只在資料卷第一次建立時執行）
--
-- 一個 PostgreSQL 16 實例、每個服務一個 database（database-per-service），
-- 每個服務兩個角色：
--   <svc>_owner — database 擁有者，執行 golang-migrate（DDL）
--   <svc>_app   — 服務執行期使用（DML only；ledger 的 journals/entries 連 UPDATE/DELETE 都沒有）
-- 生產環境由 Terraform / Vault 動態憑證建立同名角色；密碼僅限本機使用。
--
-- 連線字串範例（本機）：
--   PG_MERCHANT_DATABASE_URL=postgres://merchant_app:merchant_app@localhost:5432/pg_merchant?sslmode=disable
--   migrate -path migrations/merchant -database "postgres://merchant_owner:merchant_owner@localhost:5432/pg_merchant?sslmode=disable" up
-- =============================================================================

\set ON_ERROR_STOP on

-- ---------- roles ----------
CREATE ROLE merchant_owner LOGIN PASSWORD 'merchant_owner';
CREATE ROLE merchant_app   LOGIN PASSWORD 'merchant_app';

CREATE ROLE payment_owner  LOGIN PASSWORD 'payment_owner';
CREATE ROLE payment_app    LOGIN PASSWORD 'payment_app';

CREATE ROLE ledger_owner   LOGIN PASSWORD 'ledger_owner';
CREATE ROLE ledger_app     LOGIN PASSWORD 'ledger_app';

CREATE ROLE webhook_owner  LOGIN PASSWORD 'webhook_owner';
CREATE ROLE webhook_app    LOGIN PASSWORD 'webhook_app';

CREATE ROLE recon_owner    LOGIN PASSWORD 'recon_owner';
CREATE ROLE recon_app      LOGIN PASSWORD 'recon_app';

-- 唯讀角色（Grafana / 報表 / 排錯），每個 DB 都授 SELECT
CREATE ROLE reporting_ro    LOGIN PASSWORD 'reporting_ro';

-- ---------- databases ----------
CREATE DATABASE pg_merchant OWNER merchant_owner ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
CREATE DATABASE pg_payment  OWNER payment_owner  ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
CREATE DATABASE pg_ledger   OWNER ledger_owner   ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
CREATE DATABASE pg_webhook  OWNER webhook_owner  ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
CREATE DATABASE pg_recon    OWNER recon_owner    ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;

-- 各服務角色只能連自己的 DB
REVOKE CONNECT ON DATABASE pg_merchant, pg_payment, pg_ledger, pg_webhook, pg_recon FROM PUBLIC;
GRANT  CONNECT ON DATABASE pg_merchant TO merchant_owner, merchant_app, reporting_ro;
GRANT  CONNECT ON DATABASE pg_payment  TO payment_owner,  payment_app,  reporting_ro;
GRANT  CONNECT ON DATABASE pg_ledger   TO ledger_owner,   ledger_app,   reporting_ro;
GRANT  CONNECT ON DATABASE pg_webhook  TO webhook_owner,  webhook_app,  reporting_ro;
GRANT  CONNECT ON DATABASE pg_recon    TO recon_owner,    recon_app,    reporting_ro;

-- ---------- per-database schema privileges ----------
-- 模式：owner 擁有 public schema；app 角色取得 USAGE，並透過 ALTER DEFAULT PRIVILEGES
-- 讓 owner 之後建立（migration）的表/序列/函式自動授權給 app 與 readonly。
-- 注意：ALTER DEFAULT PRIVILEGES 只影響「指定角色建立」的物件，所以要 FOR ROLE <owner>。

\connect pg_merchant
ALTER SCHEMA public OWNER TO merchant_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO merchant_app, reporting_ro;
ALTER DEFAULT PRIVILEGES FOR ROLE merchant_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO merchant_app;
ALTER DEFAULT PRIVILEGES FOR ROLE merchant_owner IN SCHEMA public GRANT USAGE, SELECT                 ON SEQUENCES TO merchant_app;
ALTER DEFAULT PRIVILEGES FOR ROLE merchant_owner IN SCHEMA public GRANT EXECUTE                       ON FUNCTIONS TO merchant_app;
ALTER DEFAULT PRIVILEGES FOR ROLE merchant_owner IN SCHEMA public GRANT SELECT                        ON TABLES    TO reporting_ro;

\connect pg_payment
ALTER SCHEMA public OWNER TO payment_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO payment_app, reporting_ro;
ALTER DEFAULT PRIVILEGES FOR ROLE payment_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO payment_app;
ALTER DEFAULT PRIVILEGES FOR ROLE payment_owner IN SCHEMA public GRANT USAGE, SELECT                 ON SEQUENCES TO payment_app;
ALTER DEFAULT PRIVILEGES FOR ROLE payment_owner IN SCHEMA public GRANT EXECUTE                       ON FUNCTIONS TO payment_app;
ALTER DEFAULT PRIVILEGES FOR ROLE payment_owner IN SCHEMA public GRANT SELECT                        ON TABLES    TO reporting_ro;

\connect pg_ledger
ALTER SCHEMA public OWNER TO ledger_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO ledger_app, reporting_ro;
-- ledger_app 預設只有 SELECT/INSERT/UPDATE；migration 0002_entries 會再 REVOKE journals/entries 的 UPDATE/DELETE。
-- UPDATE 仍需要：accounts、balances（trigger 以表擁有者以外的身分執行時需要 app 具備權限）、processed_events、outbox。
ALTER DEFAULT PRIVILEGES FOR ROLE ledger_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO ledger_app;
ALTER DEFAULT PRIVILEGES FOR ROLE ledger_owner IN SCHEMA public GRANT USAGE, SELECT                 ON SEQUENCES TO ledger_app;
ALTER DEFAULT PRIVILEGES FOR ROLE ledger_owner IN SCHEMA public GRANT EXECUTE                       ON FUNCTIONS TO ledger_app;
ALTER DEFAULT PRIVILEGES FOR ROLE ledger_owner IN SCHEMA public GRANT SELECT                        ON TABLES    TO reporting_ro;

\connect pg_webhook
ALTER SCHEMA public OWNER TO webhook_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO webhook_app, reporting_ro;
ALTER DEFAULT PRIVILEGES FOR ROLE webhook_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO webhook_app;
ALTER DEFAULT PRIVILEGES FOR ROLE webhook_owner IN SCHEMA public GRANT USAGE, SELECT                 ON SEQUENCES TO webhook_app;
ALTER DEFAULT PRIVILEGES FOR ROLE webhook_owner IN SCHEMA public GRANT EXECUTE                       ON FUNCTIONS TO webhook_app;
ALTER DEFAULT PRIVILEGES FOR ROLE webhook_owner IN SCHEMA public GRANT SELECT                        ON TABLES    TO reporting_ro;

\connect pg_recon
ALTER SCHEMA public OWNER TO recon_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO recon_app, reporting_ro;
ALTER DEFAULT PRIVILEGES FOR ROLE recon_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO recon_app;
ALTER DEFAULT PRIVILEGES FOR ROLE recon_owner IN SCHEMA public GRANT USAGE, SELECT                 ON SEQUENCES TO recon_app;
ALTER DEFAULT PRIVILEGES FOR ROLE recon_owner IN SCHEMA public GRANT EXECUTE                       ON FUNCTIONS TO recon_app;
ALTER DEFAULT PRIVILEGES FOR ROLE recon_owner IN SCHEMA public GRANT SELECT                        ON TABLES    TO reporting_ro;

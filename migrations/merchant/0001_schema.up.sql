-- =============================================================================
-- merchant-service :: 0001_schema  (database: pg_merchant)
--
-- 依 docs/01-architecture.md：
--   * database-per-service，本檔只在 pg_merchant 執行
--   * 主鍵 uuid（應用層產生 UUIDv7，DB 端 gen_random_uuid() 備援）
--   * 對外 ID 為 public_id（mch_... / we_...）
--   * 狀態用 text + CHECK，不用 enum type（方便演進）
--   * 金額/幣別規範：currency char(3) 且為 ISO-4217 大寫三碼
--
-- 注意：golang-migrate 會把整個檔案當成單一 simple-query 送出，等同包在一個
-- 隱含交易裡；因此這裡不可使用 CREATE INDEX CONCURRENTLY（初始化時表是空的，
-- 也沒有必要）。之後對已上線大表加索引請另開 migration 並使用
-- `-- +migrate NoTransaction` 等價作法（見 docs/04-data-model.md §9）。
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 共用 trigger function：自動維護 updated_at
-- （version 欄位由應用層在 UPDATE ... WHERE version = $n 時自行 +1，不在 DB 端自動
--   遞增，避免雙重遞增破壞樂觀鎖語意）
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- -----------------------------------------------------------------------------
-- merchants — 商戶主檔（多租戶根實體）
-- 預期量：數千 ~ 數萬列，成長緩慢
-- -----------------------------------------------------------------------------
CREATE TABLE merchants (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id        text        NOT NULL,                       -- mch_xxx，對外識別
    name             text        NOT NULL,
    status           text        NOT NULL DEFAULT 'pending'
                                 CHECK (status IN ('pending', 'active', 'suspended', 'closed')),
    country          char(2)     NOT NULL CHECK (country ~ '^[A-Z]{2}$'),          -- ISO-3166-1 alpha-2
    default_currency char(3)     NOT NULL CHECK (default_currency ~ '^[A-Z]{3}$'), -- ISO-4217
    settings         jsonb       NOT NULL DEFAULT '{}',          -- 商戶層級設定（capture 預設、3DS 偏好、statement descriptor 預設…）
    metadata         jsonb       NOT NULL DEFAULT '{}',          -- 商戶自訂 key/value
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    version          integer     NOT NULL DEFAULT 0,             -- 樂觀鎖
    CONSTRAINT merchants_public_id_key UNIQUE (public_id),
    CONSTRAINT merchants_public_id_format CHECK (public_id ~ '^mch_[A-Za-z0-9]+$')
);

CREATE INDEX merchants_status_created_idx ON merchants (status, created_at DESC);

CREATE TRIGGER merchants_set_updated_at
    BEFORE UPDATE ON merchants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE  merchants IS '商戶主檔；所有其他服務的 merchant_id 都指向這裡的 id（跨庫不建 FK）';
COMMENT ON COLUMN merchants.settings IS '商戶層級設定，schema 由 internal/merchant/domain 定義並驗證';

-- -----------------------------------------------------------------------------
-- api_keys — 商戶 API Key。只存 Argon2id hash，明文只在建立當下回傳一次。
-- 查詢路徑：api-gateway 從 Bearer token 取出 prefix → 以 prefix 找到唯一一列 →
--           用 Argon2id 驗證 key_hash → 檢查 revoked_at / expires_at。
-- 預期量：每商戶數把，總量 < 100k
-- -----------------------------------------------------------------------------
CREATE TABLE api_keys (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id  uuid        NOT NULL REFERENCES merchants (id),
    prefix       text        NOT NULL,                            -- 例：pk_live_ab12cd34（可顯示、可查詢）
    key_hash     text        NOT NULL,                            -- Argon2id encoded string，絕不存明文
    mode         text        NOT NULL CHECK (mode IN ('live', 'test')),
    name         text,                                            -- 商戶自訂名稱（例：「ERP 整合」）
    scopes       text[]      NOT NULL DEFAULT '{}',               -- 例：{payments:write, refunds:write, payments:read}
    last_used_at timestamptz,                                     -- 由 gateway 以低頻（每分鐘一次）更新，避免熱寫
    expires_at   timestamptz,
    revoked_at   timestamptz,
    metadata     jsonb       NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT api_keys_prefix_key UNIQUE (prefix),
    CONSTRAINT api_keys_prefix_mode CHECK (
        (mode = 'live' AND prefix LIKE 'pk\_live\_%') OR
        (mode = 'test' AND prefix LIKE 'pk\_test\_%')
    )
);

-- 列出商戶「有效」key 的常用查詢
CREATE INDEX api_keys_merchant_active_idx
    ON api_keys (merchant_id, created_at DESC)
    WHERE revoked_at IS NULL;

CREATE TRIGGER api_keys_set_updated_at
    BEFORE UPDATE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON COLUMN api_keys.key_hash IS 'Argon2id（見 06-security-compliance.md）；不可存任何可逆形式';

-- -----------------------------------------------------------------------------
-- webhook_endpoints — 商戶接收通知的端點。
-- secret_current / secret_previous 以應用層 envelope encryption（Vault transit 或
-- KMS）加密後存放 ciphertext；輪替時 current → previous，兩把同時有效一段時間。
-- 預期量：每商戶 1~5 個
-- -----------------------------------------------------------------------------
CREATE TABLE webhook_endpoints (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_id         text        NOT NULL,                       -- we_xxx
    merchant_id       uuid        NOT NULL REFERENCES merchants (id),
    url               text        NOT NULL,
    description       text,
    secret_current    text        NOT NULL,                       -- ciphertext（應用層加密）
    secret_previous   text,                                       -- 輪替後保留一段時間
    secret_rotated_at timestamptz,
    enabled_events    text[]      NOT NULL DEFAULT '{*}',         -- 事件白名單；'*' 表示全部
    status            text        NOT NULL DEFAULT 'enabled'
                                  CHECK (status IN ('enabled', 'disabled')),
    metadata          jsonb       NOT NULL DEFAULT '{}',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    version           integer     NOT NULL DEFAULT 0,
    CONSTRAINT webhook_endpoints_public_id_key UNIQUE (public_id),
    CONSTRAINT webhook_endpoints_public_id_format CHECK (public_id ~ '^we_[A-Za-z0-9]+$')
);

CREATE INDEX webhook_endpoints_merchant_idx ON webhook_endpoints (merchant_id, status);

CREATE TRIGGER webhook_endpoints_set_updated_at
    BEFORE UPDATE ON webhook_endpoints
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE webhook_endpoints IS
    '事實來源在 merchant-service；webhook-service 透過 merchant.events 維護自己的讀模型（endpoints）';

-- -----------------------------------------------------------------------------
-- routing_preferences — 商戶的路由偏好（1:1）。rules 為有序規則陣列，
-- 由 payment-service 透過 gRPC GetRoutingPreferences 讀取並以 version 快取失效。
-- -----------------------------------------------------------------------------
CREATE TABLE routing_preferences (
    merchant_id uuid        PRIMARY KEY REFERENCES merchants (id),
    rules       jsonb       NOT NULL DEFAULT '[]',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    version     integer     NOT NULL DEFAULT 0,
    CONSTRAINT routing_preferences_rules_is_array CHECK (jsonb_typeof(rules) = 'array')
);

CREATE TRIGGER routing_preferences_set_updated_at
    BEFORE UPDATE ON routing_preferences
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON COLUMN routing_preferences.rules IS
    '例：[{"if":{"currency":"TWD"},"then":{"providers":["ecpay","stripe"]}}, ...]，schema 由 domain 層驗證';

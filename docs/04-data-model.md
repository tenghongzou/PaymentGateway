# PaymentGateway — 資料模型（Data Model）

> 本文件描述每個服務的 PostgreSQL schema、索引設計理由、分割與歸檔策略、冪等與樂觀鎖用法、帳本的 DB 級不變條件，以及備份/連線池/權限建議。
> 權威來源是 `migrations/<service>/NNNN_*.up.sql`；本文與 SQL 不一致時以 SQL 為準並回頭修文件。
> 上位決策見 `docs/01-architecture.md`（database-per-service、PostgreSQL 16、golang-migrate 純 SQL、金額 int64 最小單位、outbox、append-only 帳本）。

## 0. 通用規範（所有服務）

| 項目 | 規範 |
|---|---|
| 主鍵 | `id uuid PRIMARY KEY DEFAULT gen_random_uuid()`。應用層產生 **UUIDv7**（時間有序，B-tree 友善）；DB 預設值只是備援。 |
| 對外 ID | `public_id text NOT NULL UNIQUE`，帶前綴：`mch_` `we_` `pay_` `re_` `dp_` `jrn_` `rr_`。API 只暴露 public_id；內部 FK 用 uuid。格式以 CHECK 強制。 |
| 金額 | `bigint NOT NULL CHECK (>= 0)`，最小貨幣單位（`pkg/money`）。分錄金額 `> 0`，方向另存。 |
| 幣別 | `char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$')`（ISO-4217 大寫）。 |
| 狀態 | `text` + `CHECK (status IN (...))`，**不用 enum type**：新增狀態只需 `ALTER TABLE ... DROP CONSTRAINT / ADD CONSTRAINT`（可 `NOT VALID` 後 `VALIDATE`），不鎖表重寫。 |
| 時間 | 一律 `timestamptz`；`created_at NOT NULL DEFAULT now()`；可變表有 `updated_at`（`set_updated_at()` trigger 維護）。分割邊界以 UTC 計。 |
| 樂觀鎖 | 可變聚合根有 `version integer NOT NULL DEFAULT 0`，由**應用層**在 UPDATE 時 `version = version + 1 ... WHERE version = $expected`（見 §6）。trigger 不自動遞增，避免雙重遞增。 |
| 多租戶 | 幾乎所有表有 `merchant_id uuid NOT NULL`，並作為複合索引第一欄。跨庫**不建 FK**。 |
| metadata | `jsonb NOT NULL DEFAULT '{}'`，商戶自訂欄位；不建索引（需要時加 GIN）。 |
| outbox / processed_events | 每個有 DB 的服務都有（§7）。 |
| golang-migrate | 檔名 `NNNN_name.up.sql` / `.down.sql`，4 位數序號。每個檔案被當成**單一 simple query** 送出，即隱含一個交易；因此 migration 內**不可**用 `CREATE INDEX CONCURRENTLY`、`VACUUM` 等不能在交易中執行的語句（見 §9.3）。 |

schema 一律使用各 DB 的 `public`（database-per-service 下不需要額外 schema 隔離）。

---

## 1. merchant-service（`pg_merchant`）

### 1.1 ER 圖

```mermaid
erDiagram
    merchants ||--o{ api_keys : "owns"
    merchants ||--o{ webhook_endpoints : "registers"
    merchants ||--o| routing_preferences : "configures"

    merchants {
        uuid id PK
        text public_id UK "mch_..."
        text name
        text status "pending|active|suspended|closed"
        char2 country
        char3 default_currency
        jsonb settings
        jsonb metadata
        timestamptz created_at
        timestamptz updated_at
        int version
    }
    api_keys {
        uuid id PK
        uuid merchant_id FK
        text prefix UK "pk_live_ab12cd34"
        text key_hash "Argon2id"
        text mode "live|test"
        text name
        text_array scopes
        timestamptz last_used_at
        timestamptz expires_at
        timestamptz revoked_at
        jsonb metadata
        timestamptz created_at
        timestamptz updated_at
    }
    webhook_endpoints {
        uuid id PK
        text public_id UK "we_..."
        uuid merchant_id FK
        text url
        text secret_current "ciphertext"
        text secret_previous "ciphertext"
        timestamptz secret_rotated_at
        text_array enabled_events
        text status "enabled|disabled"
        jsonb metadata
        timestamptz created_at
        timestamptz updated_at
        int version
    }
    routing_preferences {
        uuid merchant_id PK
        jsonb rules "ordered array"
        timestamptz created_at
        timestamptz updated_at
        int version
    }
    outbox {
        uuid id PK
        text aggregate_type
        text aggregate_id
        text event_type
        bytea payload "protobuf"
        jsonb headers
        timestamptz created_at
        timestamptz published_at
        int attempts
        text last_error
    }
    processed_events {
        uuid event_id PK
        text consumer PK
        timestamptz processed_at
    }
```

### 1.2 表說明

| 表 | 重點欄位 | 索引與理由 | 預期量 |
|---|---|---|---|
| `merchants` | `status`、`country`、`default_currency`、`settings`（商戶層級預設：capture 方式、3DS 偏好…） | `UNIQUE(public_id)`；`(status, created_at DESC)` 供後台列表 | 數千～數萬列，成長緩慢 |
| `api_keys` | `prefix`（可顯示、可查詢的前 16 碼）、`key_hash`（Argon2id，**不可逆**）、`mode`、`scopes[]`、`expires_at`、`revoked_at` | `UNIQUE(prefix)`：api-gateway 驗證路徑是「從 Bearer 取 prefix → 單列查詢 → Argon2id 比對」；`(merchant_id, created_at DESC) WHERE revoked_at IS NULL` 供列出有效金鑰。`CHECK` 強制 `mode` 與前綴一致。`last_used_at` 由 gateway 節流更新（每把 key 每分鐘最多一次）以免熱寫。 | 每商戶數把，< 100k |
| `webhook_endpoints` | `url`、`secret_current/previous`（應用層 envelope encryption 後的 ciphertext；Vault transit 或 KMS）、`secret_rotated_at`、`enabled_events[]`（`{*}` = 全部）、`status` | `(merchant_id, status)` | 每商戶 1～5 個 |
| `routing_preferences` | `merchant_id` 即 PK（1:1）、`rules`（有序 JSON 陣列，`CHECK jsonb_typeof = 'array'`）、`version`（payment-service 以此做快取失效） | 無額外索引 | = 商戶數 |

事件：`merchant.created/updated/suspended`、`api_key.created/revoked`、`webhook_endpoint.created/updated/deleted/secret_rotated`、`routing_preferences.updated` → topic `merchant.events`（key = merchant_id）。

---

## 2. payment-service（`pg_payment`）

### 2.1 ER 圖

```mermaid
erDiagram
    payments ||--o{ payment_attempts : "tries provider"
    payments ||--o{ payment_events : "emits (append-only, partitioned)"
    payments ||--o{ refunds : "refunded by"
    payments ||--o{ disputes : "disputed by"

    payments {
        uuid id PK
        text public_id UK "pay_..."
        uuid merchant_id
        text idempotency_key "UNIQUE with merchant_id"
        text idempotency_request_hash
        bigint amount
        char3 currency
        text capture_method "automatic|manual"
        text status "created..chargeback_lost"
        bigint amount_authorized
        bigint amount_captured
        bigint amount_refunded
        bigint amount_refund_pending
        text payment_method_type
        jsonb payment_method_details "last4/brand/exp only"
        jsonb customer
        text description
        text statement_descriptor "<=22"
        text return_url
        text selected_provider
        text provider_reference
        text failure_category
        text failure_code
        text failure_message
        timestamptz expires_at
        timestamptz auth_expires_at
        text void_reason "merchant_request|authorization_expired|capture_failed_cleanup|risk_decline"
        timestamptz authorized_at
        timestamptz captured_at
        timestamptz voided_at
        jsonb metadata
        timestamptz created_at
        timestamptz updated_at
        int version
    }
    payment_attempts {
        uuid id PK
        uuid payment_id FK
        uuid merchant_id
        int attempt_no "UNIQUE with payment_id"
        text operation "authorize|capture|void|status_sync"
        text provider
        text provider_reference
        text status "pending|approved|requires_action|declined|unavailable|unknown"
        text error_category
        text error_code
        text error_message
        jsonb request_snapshot "masked"
        jsonb response_snapshot "masked"
        int latency_ms
        timestamptz created_at
        timestamptz completed_at
    }
    payment_events {
        uuid id PK
        timestamptz created_at PK "partition key"
        uuid payment_id FK
        uuid merchant_id
        int seq "= payments.version after transition"
        text event_type
        text from_status
        text to_status
        jsonb payload
        text actor
        text trace_id
    }
    refunds {
        uuid id PK
        text public_id UK "re_..."
        uuid payment_id FK
        uuid merchant_id
        text idempotency_key "UNIQUE with merchant_id"
        bigint amount
        char3 currency
        text status "pending|succeeded|failed"
        text reason
        text provider
        text provider_reference
        text failure_code
        text failure_message
        jsonb metadata
        timestamptz created_at
        timestamptz updated_at
        timestamptz succeeded_at
        int version
    }
    disputes {
        uuid id PK
        text public_id UK "dp_..."
        uuid payment_id FK
        uuid merchant_id
        text provider
        text provider_dispute_id "UNIQUE with provider"
        bigint amount
        char3 currency
        text status "open|evidence_submitted|under_review|won|lost|closed"
        text reason
        timestamptz evidence_due_at
        jsonb evidence
        timestamptz evidence_submitted_at
        jsonb metadata
        timestamptz created_at
        timestamptz updated_at
        timestamptz closed_at
        int version
    }
```
（`outbox`、`processed_events` 結構與 §1 相同，略。）

### 2.2 `payments`

狀態值：`created, requires_action, authorized, captured, partially_refunded, refunded, voided, failed, expired, disputed, chargeback_won, chargeback_lost`（`01-architecture.md §5.2` 的集合 + Tech Lead 裁決新增的獨立終態 `expired`）。

逾時語意：
- `created` / `requires_action` 在 `expires_at` 前未完成 → **`expired`**（終態；與 PSP 拒絕的 `failed` 區分，方便報表與商戶除錯）。
- `authorized` 逾期（`auth_expires_at`）→ 先呼叫 PSP `Void` → **`voided`**，`void_reason = 'authorization_expired'`。
- `void_reason`（`merchant_request` / `authorization_expired` / `capture_failed_cleanup` / `risk_decline`）只允許在 `voided` 狀態有值（CHECK `payments_void_reason_state`）；`voided_at` 記錄時間。

**金額欄位與 DB 級防超額 CHECK**

```
amount_authorized <= amount
amount_captured   <= amount_authorized
amount_refunded + amount_refund_pending <= amount_captured
```

- `amount_refund_pending` 是「已建立但尚未被 PSP 確認」的退款保留額。建立退款時在同一交易：`INSERT refunds(pending)` + `UPDATE payments SET amount_refund_pending += x, version = version + 1 WHERE id = $1 AND version = $v`。超額時 CHECK 直接拋 `check_violation`，應用層轉成 `400 amount_too_large`；版本衝突（0 rows）則重讀重試（最多 3 次）。退款成功時把金額從 `pending` 搬到 `refunded`。
- `refunds` 上另有 **backstop trigger** `refunds_guard_total`：以 `FOR UPDATE` 鎖住 payment 列後驗證「所有非 failed 退款總額 ≤ amount_captured」與幣別一致。主要保證仍在應用層 + version；trigger 是縱深防禦（例如人工修資料）。
- `payments_no_pan` CHECK：`payment_method_details` 不得含 `number/pan/card_number/cvc/cvv/...` 等 key，搭配應用層白名單（`last4, brand, exp_month, exp_year, funding, wallet, fingerprint`）。本系統不接觸 PAN（SAQ-A / A-EP）。
- `expires_at`：`created`/`requires_action` 的完成期限（= `05-flows-and-sequences.md` 的 `action_expires_at`），逾時 → `expired`；`auth_expires_at`：PSP 授權有效期，`void` 工作在到期前 1 小時執行 → `voided (authorization_expired)`。

**索引**

| 索引 | 用途 |
|---|---|
| `UNIQUE (merchant_id, idempotency_key)` | 冪等最後防線（§6.2） |
| `UNIQUE (public_id)` | API 以 `pay_...` 查詢 |
| `(merchant_id, created_at DESC, id DESC)` | 商戶列表與 keyset 分頁（cursor = `(created_at, id)`） |
| `(merchant_id, status, created_at DESC)` | 依狀態篩選 |
| `(selected_provider, provider_reference) WHERE provider_reference IS NOT NULL` | PSP webhook 回查、對帳 |
| `(expires_at) WHERE status IN ('created','requires_action')` | expire sweeper（→ `expired`）；partial index 只含進行中的少量列。`authorized` 不在此索引，由下一個索引處理 |
| `(auth_expires_at) WHERE status = 'authorized'` | void sweeper（→ `voided`, `authorization_expired`） |
| `(captured_at) WHERE captured_at IS NOT NULL` | 報表 / 結算區間掃描 |

**預期量**：初期 500 TPS 峰值、平均約 50～100 TPS → 每月 1.3～2.6 億列上限、實際初期每月數百萬列。每列約 1 KB（含 jsonb）→ 一年 < 100 GB。

**為何 v1 不分割 `payments`**：`UNIQUE(merchant_id, idempotency_key)` 與 `UNIQUE(public_id)` 必須包含分割鍵，會弱化跨月唯一性；`refunds/disputes/payment_attempts` 的 FK 也需改為複合鍵。單表在 1～2 億列內以上述索引運作良好。超過後以「新建分割表 + 雙寫 + 背景回填 + 切換」零停機遷移（§5.3）。

### 2.3 `payment_attempts`

每次對 provider 的 `Authorize/Capture/Void/GetPaymentStatus` 一列，`UNIQUE(payment_id, attempt_no)`。`status` 有六個值（與 SQL CHECK 一致）：`pending / requires_action / approved / declined / unavailable / unknown`，採 failover 語意：`pending` 已送出等待結果；`requires_action` PSP 要求 3DS 等額外驗證；`approved` 成功；`declined` PSP 明確拒絕（**不** failover）；`unavailable` PSP 故障/限流（可 failover）；`unknown` 逾時結果不明（需 `GetPaymentStatus` 查證後才能決定）。`request/response_snapshot` 已遮罩（不含 PAN、不含 PSP secret）。索引 `(provider, created_at DESC)` 供 provider 健康度/延遲統計（`pg_provider_latency_seconds` 的離線驗證）。量 ≈ payments × 1.1～1.5。

### 2.4 `payment_events`（append-only，按月分割）

- 每次狀態轉移在**同一交易**寫一列 + 一列 outbox。`seq` = 轉移後的 `payments.version`，因此同一 payment 內嚴格遞增且由樂觀鎖保證唯一（DB 上 `UNIQUE(payment_id, seq, created_at)` 只能保證分割內唯一）。
- `PRIMARY KEY (id, created_at)`、`PARTITION BY RANGE (created_at)`，預設分割 `payment_events_default` + 初始 `payment_events_2026_08`、`_2026_09`。
- `BEFORE UPDATE OR DELETE` row trigger `reject_mutation()` 拒絕修改；分割表上的 row trigger 會自動複製到所有現有與未來的分割（已驗證）。
- 索引：`(payment_id, seq)` 重播；`(merchant_id, created_at DESC)` 稽核時間軸；`(event_type, created_at DESC)` 統計。
- 量 ≈ payments × 3～5；保存：線上 13 個月、冷儲存 7 年（§5）。

### 2.5 `refunds` / `disputes`

- `refunds`：`pending → succeeded | failed`；`UNIQUE(merchant_id, idempotency_key)`；`(payment_id)`、`(merchant_id, created_at DESC, id DESC)`、`(provider, provider_reference)`。
- `disputes`：由 PSP webhook 建立；`UNIQUE(provider, provider_dispute_id)` 讓 PSP 重送同一 dispute 事件冪等；`(evidence_due_at) WHERE status = 'open'` 供證據截止提醒。`evidence` 只存描述與檔案參照（物件儲存 URI），不存檔案本體。

---

## 3. ledger-service（`pg_ledger`）

### 3.1 ER 圖

```mermaid
erDiagram
    accounts ||--|| balances : "has current"
    accounts ||--o{ balance_snapshots : "daily"
    accounts ||--o{ entries : "posted to"
    journals ||--|{ entries : "contains (>=2)"
    journals |o--o{ journals : "reversal_of"

    accounts {
        uuid id PK
        uuid merchant_id "NULL = system account"
        text code "see 02-domain-and-ledger.md section 7.1"
        text name
        text type "asset|liability|revenue|expense"
        text normal_balance "debit|credit"
        char3 currency
        text status "active|frozen|closed"
        jsonb metadata
        timestamptz created_at
        timestamptz updated_at
        int version
    }
    journals {
        uuid id PK
        text public_id UK "jrn_..."
        uuid merchant_id
        text reference_type "payment|refund|dispute|fee|settlement|adjustment|reversal"
        text reference_id "pay_.../re_..."
        uuid event_id UK "idempotency"
        text description
        uuid reversal_of_journal_id FK
        timestamptz posted_at
        jsonb metadata
        timestamptz created_at
    }
    entries {
        uuid id PK
        timestamptz created_at PK "partition key"
        uuid journal_id FK
        uuid account_id FK
        uuid merchant_id "denormalized by trigger"
        text direction "debit|credit"
        bigint amount "> 0"
        char3 currency
    }
    balances {
        uuid account_id PK
        char3 currency
        bigint balance "signed, normal-balance positive"
        bigint entry_count
        uuid as_of_entry_id
        timestamptz updated_at
        bigint version
    }
    balance_snapshots {
        uuid account_id PK
        date snapshot_date PK
        bigint balance
        bigint entry_count
        uuid as_of_entry_id
        timestamptz created_at
    }
```

### 3.2 表說明

| 表 | 說明 | 索引 | 預期量 |
|---|---|---|---|
| `accounts` | `UNIQUE NULLS NOT DISTINCT (merchant_id, code, currency)`（PG15+，讓系統帳戶 `merchant_id IS NULL` 也受唯一約束）；`CHECK` 強制 `type` 與 `normal_balance` 的會計對應（asset/expense→debit，liability/revenue→credit）；`code` 在 SQL **沒有** CHECK，科目清單由 domain 層（`internal/ledger/domain`）驗證，見 §3.2.1 | `(merchant_id, currency)` | 商戶 × 幣別 × 3 + 系統帳戶，< 1M |
| `journals` | 一個業務事件一筆；`event_id UNIQUE` 是冪等鍵；`reversal_of_journal_id` 指向被沖銷的 journal；**無** `updated_at/version`（不可變） | `UNIQUE(event_id)`、`(merchant_id, posted_at DESC)`、`(reference_type, reference_id)` | ≈ 終態付款 + 退款 + 拒付 + 手續費；v1 不分割 |
| `entries` | 按月分割；`amount > 0`、方向由 `direction` 表示；`merchant_id` 由 trigger 從帳戶反正規化 | `(journal_id)`、`(account_id, created_at DESC)`、`(merchant_id, created_at DESC)` | journals × 2～4；帳本最大表 |
| `balances` | 每帳戶一列，`accounts` INSERT 時由 trigger 建立；`balance` 以 `normal_balance` 方向為正（負數代表反向餘額） | PK | = accounts |
| `balance_snapshots` | 每日 UTC 00:00 快照；對帳取某日餘額、漂移偵測、歸檔後重建 | PK `(account_id, snapshot_date)`、`(snapshot_date)` | accounts × 天數 |

#### 3.2.1 Chart of Accounts（科目表）

> **科目代碼清單以 `docs/02-domain-and-ledger.md` §7.1 為準**；本表只是對應到 `accounts` 欄位的摘要。新增科目不需改 migration（`code` 無 CHECK），只需更新 02 §7.1 與 domain 層的科目常數。

`accounts.code` 存 02 §7.1 的 `kind`，provider 維度的科目以 `kind:<provider>` 區分（例：`psp_receivable:stripe`）；`merchant_id` 對應 02 的 `owner = merchant`，`NULL` 對應 `owner = platform / provider`（系統帳戶）。

| 02 code | `accounts.code`（kind） | `type` | `normal_balance` | `merchant_id` | 用途 |
|---|---|---|---|---|---|
| `1100` | `psp_receivable:<provider>` | asset | debit | NULL | PSP 已請款、尚未撥付給平台的應收款（毛額） |
| `1200` | `bank_cash:<bank_account>` | asset | debit | NULL | PSP 撥付進來的現金 |
| `1900` | `settlement_suspense:<provider>` | asset | debit | NULL | 對帳差異暫記科目；月結前必須清零 |
| `2100` | `merchant_payable` | liability | credit | 商戶 | 平台應付商戶的淨額（商戶可用餘額） |
| `2200` | `refund_clearing` | liability | credit | 商戶 | 已從商戶餘額扣除、尚待 PSP 確認的退款 |
| `2300` | `chargeback_reserve` | liability | credit | 商戶 | 因爭議凍結的商戶資金 |
| `4100` | `fee_revenue` | revenue | credit | NULL | 平台向商戶收取的交易手續費 |
| `4200` | `chargeback_fee_revenue` | revenue | credit | NULL | 向商戶收取的拒付處理費 |
| `5100` | `psp_fee_expense:<provider>` | expense | debit | NULL | 平台付給 PSP 的成本（結算時入帳） |
| `5200` | `chargeback_fee_expense:<provider>` | expense | debit | NULL | PSP 向平台收取的拒付費 |

01 文件列出的五個核心科目為 `merchant_payable`、`psp_receivable`、`fee_revenue`、`refund_clearing`、`chargeback_reserve`；其餘五個（`bank_cash`、`settlement_suspense`、`chargeback_fee_revenue`、`psp_fee_expense`、`chargeback_fee_expense`）是完成結算、拒付與對帳差異借貸平衡所需的配套科目。帳戶以 lazy create 方式在第一次過帳時建立（同交易 `INSERT ... ON CONFLICT DO NOTHING`，`accounts_create_balance` trigger 同時建立 `balances` 列）。分錄範本（J-CAP、J-REF-*、J-CB-*、J-STL、J-STL-DIFF、J-REV）見 02 §7.3。

典型 journal（02 的 **J-CAP**，`payment.captured`，TWD 10,000、手續費 300）：

| account | direction | amount |
|---|---|---|
| `psp_receivable:stripe` (asset, system) | debit | 10000 |
| `merchant_payable` (liability, merchant) | credit | 9700 |
| `fee_revenue` (revenue, system) | credit | 300 |

### 3.3 DB 級不變條件（trigger 實作）

| 不變條件 | 實作 | 時機 |
|---|---|---|
| **I1 借貸平衡**：同一 journal `SUM(debit) = SUM(credit)` | `CONSTRAINT TRIGGER ... DEFERRABLE INITIALLY DEFERRED` 掛在 `journals`（AFTER INSERT）與 `entries`（AFTER INSERT），共用 `assert_journal_balanced()`：以 `journal_id` 彙總 `entries`，不平衡即 `RAISE ... integrity_constraint_violation` | **COMMIT 時**；允許應用層逐筆 INSERT 分錄，不需一次多值 INSERT |
| **I2 至少兩筆分錄、幣別一致** | 同一函式：`COUNT(*) >= 2`、`COUNT(DISTINCT currency) = 1`。掛在 `journals` 端可擋「只有 journal 沒分錄」 | COMMIT 時 |
| **I3 分錄幣別 = 帳戶幣別、帳戶 active** | `entries_before_insert()` BEFORE INSERT：查 `accounts`，不符即拒絕；同時填 `merchant_id` | 立即 |
| **I4 append-only** | `reject_mutation()` BEFORE UPDATE OR DELETE ON `journals`、`entries`（row trigger 自動複製到每個分割）、BEFORE TRUNCATE ON `journals`；另外 migration 對 `ledger_app` 角色 `REVOKE UPDATE, DELETE, TRUNCATE`（縱深防禦：連 trigger 都不用觸發） | 立即 |
| **餘額一致**：`balances.balance = Σ(entries)` | `entries_apply_balance()` AFTER INSERT：`UPDATE balances SET balance += ±amount, entry_count += 1, as_of_entry_id = NEW.id, version += 1`；同帳戶寫入在此 row lock 上序列化 | 立即 |
| 漂移偵測 | view `v_balance_drift`（全表掃描，排程使用）→ 指標 `pg_ledger_imbalance_total` 必須恆為 0 | 排程 |

錯帳處理：**不得** UPDATE/DELETE；建立 `reference_type = 'reversal'`、`reversal_of_journal_id = <原 journal>` 的新 journal，分錄方向相反。

熱點說明：`merchant_payable` 的 `balances` 列在高 TPS 下會成為鎖競爭點（單一大商戶每秒數百次入帳）。v1 接受（單列 UPDATE 約 <1ms）；Phase 2 若成為瓶頸，改為「balances 非同步投影」：移除 `entries_apply_balance` trigger，由 consumer 批次彙總 `entries` 更新 `balances`（最終一致，`as_of_entry_id` 標示進度）。

### 3.4 `ensure_monthly_partition`

`payment` 與 `ledger` DB 各有 `ensure_monthly_partition(parent text, month date)`，幂等建立 `<parent>_YYYY_MM`。需以 **owner 角色**執行（建表需 schema CREATE 權限）：

```sql
-- CronJob（每日，owner 連線）
SELECT ensure_monthly_partition('entries',        (date_trunc('month', now()) + interval '1 month')::date);
SELECT ensure_monthly_partition('payment_events', (date_trunc('month', now()) + interval '1 month')::date);
```
新分割會自動繼承父表的 trigger 與（透過 `ALTER DEFAULT PRIVILEGES`）app 角色的 DML 權限（皆已驗證）。

---

## 4. webhook-service（`pg_webhook`）

### 4.1 ER 圖

```mermaid
erDiagram
    endpoints ||..o{ webhook_deliveries : "target (no FK, projection)"
    webhook_events ||--o{ webhook_deliveries : "fan-out per endpoint"
    webhook_deliveries ||--o{ webhook_delivery_attempts : "http attempts"

    endpoints {
        uuid id PK "= merchant.webhook_endpoints.id"
        uuid merchant_id
        text url
        text secret_current "ciphertext"
        text secret_previous
        timestamptz secret_rotated_at
        text_array enabled_events
        text status "enabled|disabled|deleted"
        int source_version
        timestamptz created_at
        timestamptz updated_at
    }
    webhook_events {
        uuid event_id PK "= source outbox.id"
        uuid merchant_id
        text event_type
        text resource_type
        text resource_id "pay_..."
        jsonb payload "external JSON"
        timestamptz occurred_at
        timestamptz created_at
    }
    webhook_deliveries {
        uuid id PK
        uuid event_id FK "UNIQUE with endpoint_id"
        uuid endpoint_id
        uuid merchant_id
        int attempt_no
        text status "pending|in_flight|succeeded|failed|dead_letter|canceled"
        timestamptz next_attempt_at
        timestamptz last_attempt_at
        int last_response_status
        text last_response_body "<=4KB"
        text last_error
        timestamptz delivered_at
        timestamptz created_at
        timestamptz updated_at
        int version
    }
    webhook_delivery_attempts {
        uuid id PK
        uuid delivery_id FK
        int attempt_no "UNIQUE with delivery_id"
        int response_status
        text response_body "<=4KB"
        text error
        int duration_ms
        timestamptz attempted_at
    }
```

### 4.2 表說明

| 表 | 說明 | 索引 | 預期量 / 保存 |
|---|---|---|---|
| `endpoints` | `pg_merchant.webhook_endpoints` 的**讀模型**，由 `merchant.events` 投影；`source_version` 丟棄亂序舊事件。不建 FK 指向它（可 truncate 重播重建）。 | `(merchant_id) WHERE status = 'enabled'`（fan-out） | = 端點數 |
| `webhook_events` | 每個要通知商戶的事件一列，`event_id` = 來源 outbox id（天然去重）；`payload` 是對外 JSON（已由 protobuf 轉換、脫敏） | `(merchant_id, occurred_at DESC)`、`(merchant_id, resource_id)`、`(occurred_at)` 清理 | ≈ payment_events；保留 90 天 |
| `webhook_deliveries` | `(event, endpoint)` 一列的投遞狀態機；`UNIQUE(event_id, endpoint_id)` 讓重複 fan-out 冪等 | **`(next_attempt_at) WHERE status IN ('pending','failed')`**：worker 取件查詢 `WHERE status IN ('pending','failed') AND next_attempt_at <= now() ORDER BY next_attempt_at FOR UPDATE SKIP LOCKED LIMIT 100` 走 Index Scan（已以 EXPLAIN 驗證）；partial index 只含待送列，大小與積壓量成正比而非總量。`(updated_at) WHERE status = 'in_flight'` 供 reaper 找殭屍取件。另有 `(endpoint_id, created_at DESC)`、`(merchant_id, status, created_at DESC)` 供後台 / 死信（`dead_letter`）清單 | events × 端點數，webhook DB 最大表；保留 90 天 |
| `webhook_delivery_attempts` | 每次 HTTP 嘗試的紀錄（商戶後台顯示「第 n 次：503, 1200ms」） | `UNIQUE(delivery_id, attempt_no)`、`(attempted_at)` | deliveries × 平均嘗試數；保留 30 天 |

狀態機（canonical 六個狀態：`pending, in_flight, succeeded, failed, dead_letter, canceled`）：

| 狀態 | 意義 | 轉移 |
|---|---|---|
| `pending` | 初始 / 等待（重）試 | 取件 → `in_flight` |
| `in_flight` | dispatcher 已取件、HTTP 請求進行中 | 2xx → `succeeded`；非 2xx/逾時 → `attempt_no + 1`、`next_attempt_at = now() + backoff(attempt_no)` → `failed`；`attempt_no ≥ 8`（約 24h）→ `dead_letter` |
| `failed` | 上次嘗試失敗、已排定重試（worker 取件時與 `pending` 同等看待） | 取件 → `in_flight` |
| `succeeded` | 收到 2xx（終態） | — |
| `dead_letter` | 放棄投遞（終態）；outbox 發 `webhook.delivery.dead_lettered` 告警；商戶可從後台手動重送（重設為 `pending`，`attempt_no` 不歸零） | 手動 → `pending` |
| `canceled` | 端點被刪除 / 停用時取消尚未成功的投遞（終態）；outbox 發 `webhook.delivery.canceled` | — |

取件以單一 `UPDATE ... SET status = 'in_flight' WHERE id IN (SELECT ... WHERE status IN ('pending','failed') AND next_attempt_at <= now() ORDER BY next_attempt_at FOR UPDATE SKIP LOCKED LIMIT 100) RETURNING *` 完成；`in_flight` 不在 `webhook_deliveries_due_idx` 內，取件後即離開待送索引。worker 崩潰造成的殭屍 `in_flight` 由 reaper 透過 `webhook_deliveries_in_flight_idx`（`(updated_at) WHERE status = 'in_flight'`）找出、轉回 `failed`。`webhook_delivery_attempts` 只記錄每次 HTTP 嘗試的結果，不依賴 delivery 狀態值。

分割：v1 不分割，以排程批次 DELETE（每批 5k 列、依 `occurred_at/attempted_at` 索引）清理；Phase 1 若每日投遞量 > 500 萬，改為按月分割（`UNIQUE` 需含 `created_at`）。

---

## 5. reconciliation-service（`pg_recon`）

### 5.1 ER 圖

```mermaid
erDiagram
    settlement_files ||--o{ settlement_lines : "contains"
    reconciliation_runs ||--o{ discrepancies : "produces"
    settlement_lines |o--o{ discrepancies : "evidence"
    payment_records ||..o{ discrepancies : "internal side (projection)"
    ledger_postings ||..o{ discrepancies : "ledger side (projection)"

    settlement_files {
        uuid id PK
        text provider
        text file_name
        text file_hash UK "sha256"
        text storage_uri
        date period_start
        date period_end
        int row_count
        text status "pending|importing|imported|failed"
        text error
        timestamptz imported_at
        jsonb metadata
        timestamptz created_at
        timestamptz updated_at
        int version
    }
    settlement_lines {
        uuid id PK
        uuid file_id FK
        int line_no "UNIQUE with file_id"
        text provider
        text provider_reference
        text merchant_reference
        text type "payment|refund|chargeback|fee|adjustment"
        bigint amount
        char3 currency
        timestamptz settled_at
        jsonb raw
        timestamptz created_at
    }
    payment_records {
        uuid id PK "= payment/refund/dispute id"
        text kind "payment|refund|dispute"
        text public_id UK
        uuid merchant_id
        text provider
        text provider_reference
        bigint amount
        char3 currency
        text status
        timestamptz occurred_at
        int source_seq
        timestamptz created_at
        timestamptz updated_at
    }
    ledger_postings {
        uuid journal_id PK
        uuid merchant_id
        text reference_type
        text reference_id
        bigint amount
        char3 currency
        timestamptz posted_at
        timestamptz created_at
    }
    reconciliation_runs {
        uuid id PK
        text public_id UK "rr_..."
        text provider
        timestamptz period_start
        timestamptz period_end
        text status "pending|running|completed|failed"
        int matched_count
        int unmatched_count
        jsonb summary
        text error
        timestamptz started_at
        timestamptz finished_at
        text triggered_by
        timestamptz created_at
        timestamptz updated_at
        int version
    }
    discrepancies {
        uuid id PK
        uuid run_id FK
        uuid merchant_id "nullable"
        text kind "missing_in_ledger|missing_in_psp|amount_mismatch|status_mismatch"
        text provider
        text provider_reference
        text internal_reference
        uuid settlement_line_id FK
        bigint expected_amount
        bigint actual_amount
        char3 currency
        text status "open|resolved|ignored"
        text resolution_note
        text resolved_by
        timestamptz resolved_at
        jsonb details
        timestamptz created_at
        timestamptz updated_at
        int version
    }
```

### 5.2 表說明

| 表 | 說明 | 索引 | 預期量 / 保存 |
|---|---|---|---|
| `settlement_files` | `file_hash UNIQUE`：同檔重複上傳冪等；`storage_uri` 指向原檔（物件儲存） | `(provider, created_at DESC)` | 每 PSP 每日 1 檔 |
| `settlement_lines` | 正規化後的每一列，`raw` 保留原始欄位；`amount >= 0`，方向由 `type` 決定 | `UNIQUE(file_id, line_no)`；`(provider, provider_reference, type)` 比對主鍵（同一交易有 payment 與 fee 兩列）；`(provider, settled_at)` 區間掃描 | 每日每 PSP 數萬～數十萬；線上 25 個月 |
| `payment_records` | **讀模型**（`payment.events` / `refund.events` 投影）：內部付款/退款在 PSP 端的參照與金額；`source_seq` 丟棄亂序 | `(provider, provider_reference)`、`(merchant_id, occurred_at DESC)` | ≈ payments + refunds |
| `ledger_postings` | **讀模型**（`ledger.events` 的 `journal.posted`）：journal 摘要 | `(reference_type, reference_id)`、`(merchant_id, posted_at DESC)` | ≈ journals |
| `reconciliation_runs` | provider × 期間一次執行；`summary` 存各類計數與金額 | `(provider, period_start DESC)` | 每日每 PSP 1～數次 |
| `discrepancies` | 四種 `kind`；`CHECK` 強制 `resolved/ignored` 必須有 `resolved_at` | `(run_id, kind)`；`(created_at) WHERE status = 'open'` 待辦清單；`(provider, provider_reference)` 避免跨 run 重複開單 | 比對量的 0.1～1% |

比對全部在本地讀模型上以 SQL JOIN 完成（`settlement_lines ⋈ payment_records ⋈ ledger_postings`），沒有任何跨庫查詢。

---

## 6. 樂觀鎖與冪等唯一鍵

### 6.1 樂觀鎖（`version`）

```sql
UPDATE payments
   SET status = 'captured', amount_captured = $3, captured_at = now(),
       version = version + 1                      -- 應用層遞增
 WHERE id = $1 AND version = $2;                  -- 期望版本
-- rows affected = 0 → 重讀 payment；若已在目標狀態（或更後面）視為 no-op 成功，否則回 409
```
- 適用：`payments`、`refunds`、`disputes`、`merchants`、`webhook_endpoints`、`routing_preferences`、`webhook_deliveries`、`reconciliation_runs`、`discrepancies`、`settlement_files`。
- 同一交易內同時 `INSERT payment_events (seq = 新 version)` + `INSERT outbox`；任何一步失敗整個回滾。
- `updated_at` 由 trigger 維護；`version` 不由 trigger 維護（避免雙重遞增）。
- `balances.version` 是 trigger 維護的例外（純粹給讀端做 ETag，不用於寫入衝突判斷）。

### 6.2 冪等唯一鍵

| 層級 | 機制 | 鍵 |
|---|---|---|
| api-gateway | Redis `SETNX` + `(request_hash, response)` 快取 24h | `Idempotency-Key` |
| payment-service | `UNIQUE (merchant_id, idempotency_key)` on `payments` / `refunds`；`idempotency_request_hash` 比對 payload，不同 → `409 idempotency_error` | `(merchant_id, idempotency_key)` |
| PSP 端 | 以 `payment_id + ":auth"`、`payment_id + ":cap"` 等固定字串作 PSP idempotency key | — |
| 事件消費 | `processed_events (event_id, consumer)` PK，`INSERT ... ON CONFLICT DO NOTHING` 與業務寫入同交易 | `(event_id, consumer)` |
| ledger | `journals.event_id UNIQUE`（第二道） | `event_id` |
| webhook | `webhook_events.event_id` PK、`UNIQUE (event_id, endpoint_id)` | — |
| recon | `settlement_files.file_hash UNIQUE`、`UNIQUE (file_id, line_no)` | — |
| PSP inbound | `disputes UNIQUE (provider, provider_dispute_id)` | — |

唯一鍵違反 → `23505 unique_violation`；應用層以 `pgconn.PgError.ConstraintName` 對應到領域錯誤（`pkg/pgdb` 提供 helper）。

---

## 7. Outbox 與消費端去重

每個有 DB 的服務都有：

```sql
outbox (id uuid PK, aggregate_type, aggregate_id, event_type, payload bytea, headers jsonb,
        created_at, published_at, attempts, last_error)
  INDEX (created_at) WHERE published_at IS NULL      -- relay 取件
  INDEX (published_at) WHERE published_at IS NOT NULL -- 清理
processed_events (event_id uuid, consumer text, processed_at, PRIMARY KEY (event_id, consumer))
```

- relay（`pkg/outbox`）：`SELECT ... WHERE published_at IS NULL ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 500` → produce（key = `aggregate_id`，header `event_id = id`）→ `UPDATE SET published_at = now()`。crash 在 produce 後、UPDATE 前會重送 → 消費端靠 `processed_events` 去重（at-least-once + 冪等 = effectively-once）。
- `outbox.id` 即事件的全域 `event_id`；下游 `processed_events.event_id`、`journals.event_id`、`webhook_events.event_id` 都引用它。
- 清理：`outbox` 已發佈列保留 7 天；`processed_events` 保留 30 天（需 ≥ Kafka retention，避免舊事件重播時誤判為新事件）。
- 指標 `pg_outbox_lag_seconds` = `now() - min(created_at) WHERE published_at IS NULL`。

---

## 8. 分割、歸檔與保存期限

### 8.1 已分割的表（declarative partitioning，按月，UTC）

| 表 | 分割鍵 | PK | 初始分割 | 新增方式 |
|---|---|---|---|---|
| `pg_payment.payment_events` | `created_at` | `(id, created_at)` | `_default`, `_2026_08`, `_2026_09` | `ensure_monthly_partition('payment_events', …)` |
| `pg_ledger.entries` | `created_at` | `(id, created_at)` | `_default`, `_2026_08`, `_2026_09` | `ensure_monthly_partition('entries', …)` |

運維規則：
1. CronJob 每日以 owner 角色預建「下個月」分割，確保 `*_default` 恆為空（監控 `SELECT count(*) FROM payment_events_default` 應為 0；若不為 0 表示分割沒建，且之後補建對應月份時 PG 需掃描 default 分割並取鎖）。
2. 歸檔：`ALTER TABLE entries DETACH PARTITION entries_2024_07 CONCURRENTLY` → `pg_dump -t entries_2024_07` 到物件儲存（WORM bucket）→ `DROP TABLE`。分割上的 append-only trigger 不影響 DDL。
3. 帳本歸檔後仍可重建任一帳戶任一時點餘額：`balance_snapshots` 最近快照 + 之後的 `entries`。

### 8.2 說明但未分割的大表

| 表 | 為何暫不分割 | 觸發條件 | 方案 |
|---|---|---|---|
| `payments` | 唯一鍵（idempotency、public_id）需含分割鍵；子表 FK 需改複合鍵 | > 2 億列或單表 > 500 GB | 新建 `payments_p`（按月）→ 雙寫 → 背景回填 → 切換讀 → 切換寫 → 改名 |
| `webhook_deliveries` | 保留只有 90 天，批次 DELETE 足夠 | 每日 > 500 萬投遞 | 按月分割，`UNIQUE(event_id, endpoint_id, created_at)` |
| `webhook_events` | 同上 | 同上 | 同上 |
| `settlement_lines` | 量可控 | > 5 億列 | 按 `settled_at` 月分割 |
| `journals` | 列小、查詢多為點查 | > 2 億列 | 按 `posted_at` 月分割 |

### 8.3 保存期限

| 資料 | 線上 | 冷儲存 | 依據 |
|---|---|---|---|
| `payments`, `refunds`, `disputes`, `payment_attempts` | 18 個月（終態） | 7 年 | 稽核 / 拒付時效（最長 540 天） |
| `payment_events` | 13 個月 | 7 年 | 稽核 |
| `journals`, `entries`, `balance_snapshots` | 25 個月 | 10 年 | 會計法規 |
| `webhook_events`, `webhook_deliveries` | 90 天 | — | 商戶除錯 |
| `webhook_delivery_attempts` | 30 天 | — | |
| `settlement_files/lines` | 25 個月 | 原檔 10 年 | 對帳 |
| `discrepancies`, `reconciliation_runs` | 25 個月 | 10 年 | |
| `outbox`（已發佈） | 7 天 | — | |
| `processed_events` | 30 天 | — | ≥ Kafka retention |
| `api_keys`（revoked） | 永久（只存 hash） | — | 稽核 |

刪除一律批次（`DELETE ... WHERE id IN (SELECT id ... LIMIT 5000)`）並在離峰執行；被分割的表用 DETACH/DROP 取代 DELETE。

---

## 9. 跨服務查詢策略

**沒有跨庫 JOIN、沒有跨庫 FK。** `merchant_id`、`payment_id` 等跨服務參照只是 uuid 欄位。

| 需求 | 方式 | 範例 |
|---|---|---|
| 同步、低延遲、需要最新值 | gRPC 呼叫擁有資料的服務 | payment-service 建立付款前呼叫 merchant-service `GetRoutingPreferences`（以 `version` 做本地快取失效）；api-gateway 驗 API key 呼叫 merchant-service（Redis 快取 60s） |
| 非同步、可容忍秒級延遲、需要在本地以 SQL 過濾/JOIN | 消費事件建立**讀模型（projection）** | webhook-service 的 `endpoints`；reconciliation-service 的 `payment_records`、`ledger_postings` |
| 對外列表 / 報表 | 由 api-gateway 組合多個 gRPC 回應；重度報表走 Phase 3 的資料倉儲（CDC：`wal_level = logical` 已在 compose 開啟） | `GET /v1/payments/{id}` 同時回 payment（payment-service）與 ledger balance（ledger-service） |

讀模型規則：
- 投影表有 `source_version` / `source_seq`，比較後丟棄亂序舊事件（`UPDATE ... WHERE source_version < $new`）。
- 投影可隨時 `TRUNCATE` 後從 Kafka（或來源服務的 `Replay` gRPC）重建；因此**不**對投影表建立 FK。
- 投影中的秘密（如 `endpoints.secret_current`）與來源一樣只存 ciphertext。

---

## 10. 運維：備份、PITR、連線池、權限

### 10.1 備份與 PITR
- 生產：每個服務獨立的 PostgreSQL 16 實例（或託管服務的獨立 DB cluster），**不是**同一實例多 DB（故障域與 PITR 粒度都要獨立）。
- 連續 WAL 歸檔（pgBackRest / WAL-G → 物件儲存，跨區複寫），每日 full + 每小時 incremental；RPO ≤ 5 分鐘、RTO ≤ 1 小時。
- 帳本（`pg_ledger`）與付款（`pg_payment`）額外：每日 `pg_dump --format=directory` 邏輯備份到 WORM bucket（防勒索、可單表還原）。
- 每月演練：從備份 + WAL 還原到隔離環境，跑 `v_balance_drift` 與 row count 比對，並記錄 RTO。
- 同步 standby（`synchronous_commit = remote_apply` 至少 1 個 replica）給 `pg_payment` 與 `pg_ledger`；其餘 DB 非同步 replica 即可。

### 10.2 連線池（pgbouncer）
- 每個服務前放 pgbouncer（sidecar 或獨立 deployment），`pool_mode = transaction`。
- transaction pooling 的限制：不可用 session-level 功能（`SET` 不帶 `LOCAL`、`LISTEN`、advisory lock across tx、prepared statements 需 pgbouncer ≥ 1.21 的 `max_prepared_statements`）。`pgx/v5` 設 `default_query_exec_mode = cache_describe` 或 `exec` 並開啟 pgbouncer `max_prepared_statements = 200`。
- 建議池大小（每服務）：`default_pool_size = 20`、`max_client_conn = 1000`、`reserve_pool_size = 5`；PostgreSQL `max_connections` 依 `Σ pool_size + 維運 + replica` 設定（~150），而非 compose 的 300（本機用）。
- relay worker 與 CronJob 使用**獨立**的小池（避免 `FOR UPDATE SKIP LOCKED` 長交易佔滿服務池）。
- golang-migrate 必須**直連** PostgreSQL（非經 pgbouncer），因為需要 advisory lock 與 session 狀態。

### 10.3 每服務獨立 DB 帳號與權限

| 角色 | 用途 | 權限 |
|---|---|---|
| `<svc>_owner` | 擁有 DB 與所有物件；執行 migration、建立分割（CronJob） | DB owner；`CREATE` on schema |
| `<svc>_app` | 服務執行期 | `USAGE` on schema；`SELECT/INSERT/UPDATE/DELETE` on tables（透過 `ALTER DEFAULT PRIVILEGES FOR ROLE <svc>_owner` 自動套用到新物件）；**`ledger_app` 對 `journals`/`entries` 無 UPDATE/DELETE/TRUNCATE** |
| `reporting_ro` | Grafana / 排錯 | 各 DB `SELECT` |

- 角色名**不可**以 `pg_` 開頭（PostgreSQL 保留；`CREATE ROLE pg_merchant` 會直接失敗）。
- 生產：Vault database secrets engine 發放 `<svc>_app` 的動態憑證（TTL 1h），`<svc>_owner` 只在 CI/CD migration job 與 CronJob 使用。
- 每個角色只被授予自己 DB 的 `CONNECT`（`REVOKE CONNECT ... FROM PUBLIC`）。
- 服務間 mTLS 與 DB TLS（`sslmode=verify-full`）在生產強制。

### 10.4 建議的 PostgreSQL 參數（生產，per-instance 16 vCPU / 64 GB）
`shared_buffers = 16GB`、`effective_cache_size = 48GB`、`work_mem = 32MB`、`maintenance_work_mem = 2GB`、`wal_compression = zstd`、`checkpoint_timeout = 15min`、`max_wal_size = 16GB`、`random_page_cost = 1.1`（SSD）、`log_min_duration_statement = 200ms`、`track_io_timing = on`、`autovacuum_vacuum_scale_factor = 0.02`（`payments`、`webhook_deliveries` 另以表級參數調低到 0.005）。

---

## 11. 本機開發（docker-compose）

單一 `postgres:16-alpine` 容器、五個 database。`deploy/compose/postgres/init.sql` 掛載到 `/docker-entrypoint-initdb.d/00-init.sql`，**只在資料卷第一次建立時執行**（改了 init.sql 要 `docker compose down -v`）。

init.sql 建立的物件：

| Database | Owner（跑 migration） | App 角色（服務連線） | 唯讀 |
|---|---|---|---|
| `pg_merchant` | `merchant_owner` / `merchant_owner` | `merchant_app` / `merchant_app` | `reporting_ro` |
| `pg_payment` | `payment_owner` / `payment_owner` | `payment_app` / `payment_app` | `reporting_ro` |
| `pg_ledger` | `ledger_owner` / `ledger_owner` | `ledger_app` / `ledger_app` | `reporting_ro` |
| `pg_webhook` | `webhook_owner` / `webhook_owner` | `webhook_app` / `webhook_app` | `reporting_ro` |
| `pg_recon` | `recon_owner` / `recon_owner` | `recon_app` / `recon_app` | `reporting_ro` |

（格式：帳號 / 密碼；密碼僅供本機。）每個 DB：`ALTER SCHEMA public OWNER TO <owner>`、`REVOKE CREATE ON SCHEMA public FROM PUBLIC`、`ALTER DEFAULT PRIVILEGES FOR ROLE <owner> ... TO <app>, reporting_ro`。

連線字串：
```
# 服務執行期（PG_DATABASE_URL）
postgres://payment_app:payment_app@postgres:5432/pg_payment?sslmode=disable
# migration（PG_MIGRATE_DATABASE_URL；PG_AUTO_MIGRATE=true 時服務啟動用這把跑 golang-migrate）
postgres://payment_owner:payment_owner@postgres:5432/pg_payment?sslmode=disable
```

> 目前 `deploy/compose/docker-compose.yaml` 的 `PG_DATABASE_URL` 使用 `pg_merchant:pg_merchant@.../pg_merchant` 這類角色名；`pg_` 前綴的角色名被 PostgreSQL 保留、無法建立。請改為上表的 `<svc>_owner`（本機 `PG_AUTO_MIGRATE=true` 需 DDL 權限）或拆成 `PG_DATABASE_URL`（app）+ `PG_MIGRATE_DATABASE_URL`（owner）。

手動跑 migration（需 `golang-migrate` CLI）：
```bash
migrate -path migrations/payment \
        -database "postgres://payment_owner:payment_owner@localhost:5432/pg_payment?sslmode=disable" up
migrate -path migrations/payment -database "$URL" down 1
```
或不裝工具、用 psql 模擬 golang-migrate 的「一檔一交易」語意：
```bash
for f in migrations/payment/*.up.sql; do
  psql "postgres://payment_owner:payment_owner@localhost:5432/pg_payment" -v ON_ERROR_STOP=1 -1 -f "$f"
done
```

### 11.1 Migration 撰寫規範
1. 新增 migration 永遠用下一個序號、**不要**修改已合併的檔案；每個 up 都要有對應可逆的 down。
2. 一個檔案 = 一個交易：不可放 `CREATE INDEX CONCURRENTLY`、`ALTER TYPE ... ADD VALUE`（我們不用 enum 正是為此）、`VACUUM`。上線後對大表加索引：另開 migration，檔案第一行 `-- +migrate: no-transaction`（由 `pkg/pgdb` migration helper 辨識後以 `x-multi-statement` 模式、不包交易執行）。
3. 新增 NOT NULL 欄位到大表：先 `ADD COLUMN ... DEFAULT ...`（PG11+ 不重寫表），再補 `NOT NULL`；加 CHECK 用 `NOT VALID` + 之後 `VALIDATE CONSTRAINT`。
4. 狀態值演進：`ALTER TABLE ... DROP CONSTRAINT payments_status_check, ADD CONSTRAINT payments_status_check CHECK (...) NOT VALID;` 然後 `VALIDATE`。
5. 服務程式碼必須同時相容 N 與 N-1 版 schema（expand → migrate → contract），migration 在 deploy 前跑（CI/CD job，owner 角色，直連 DB）。
6. 所有 migration 在 CI 以 testcontainers 起 PG16：up → 寫入樣本 → down → up，與 `deploy/compose/postgres/init.sql` 一起驗證。

---

## 12. 驗證紀錄（2026-08-20，PostgreSQL 16.15 / postgres:16-alpine）

- `init.sql` 以 `postgres` 超級使用者執行成功（5 DB、11 角色）。
- 2026-08-20（補充）：`payments.status` 加入獨立終態 `expired`、新增 `void_reason`（CHECK 值集 + 只在 `voided` 可有值）與 `voided_at`，expire sweeper 索引收斂為 `status IN ('created','requires_action')`；payment 的 up → 寫入測試（`expired`、`voided + authorization_expired` 通過；非 voided 帶 void_reason、未知 void_reason 被拒）→ down → up → down 驗證通過，兩個 sweeper 查詢皆走對應 partial index。
- 2026-08-20（補充）：webhook_deliveries 狀態集改為 `pending, in_flight, succeeded, failed, dead_letter, canceled` 後，webhook 的 up → 六個狀態寫入測試（舊值 `dead` 被 CHECK 拒絕）→ down → up → down 重新驗證通過；取件查詢仍走 `webhook_deliveries_due_idx`，reaper 查詢走 `webhook_deliveries_in_flight_idx`。
- 5 個服務共 12 個 up migration 以各自 `<svc>_owner` 角色、每檔單一交易（`psql -1 -v ON_ERROR_STOP=1`）套用成功；down（反序）→ 無殘留物件 → up 再次套用成功 → down 再次成功。
- 以 `<svc>_app` 角色做不變條件測試：ledger 借貸不平衡 / 無分錄 / 幣別不符 / 重複 `event_id` / UPDATE·DELETE（權限與 trigger 兩層）皆被拒絕；新分割自動繼承 4 個 trigger 與 app 權限；`v_balance_drift` 為 0 列。payment 的三條超額 CHECK、退款 backstop trigger、PAN 防呆、冪等唯一鍵、`payment_events` append-only 與分割路由皆如預期。webhook 取件查詢使用 `webhook_deliveries_due_idx`。

# ADR-0004：金額使用 `int64` 最小貨幣單位

- **Status**：Accepted（2026-08）
- **關聯**：`01-architecture.md` §2、§5.1；`pkg/money`；`09-testing-strategy.md` §5

## Context

金額是支付系統最不能出錯的資料。常見錯誤來源：

- 浮點數（`float64`）的二進位表示無法精確表達 0.1，累加與比較會出現 `0.30000000000000004`。
- 不同幣別的小數位數不同：TWD / JPY / KRW 為 0 位，USD / EUR 為 2 位，KWD / BHD / JOD 為 3 位。以「元」為單位儲存會讓每次運算都要查小數位數。
- 跨語言 / 跨系統（JSON、PSP API、DB）傳遞時的隱性轉型。
- 分攤（手續費拆分、部分退款比例）時的捨入規則不一致，導致帳本不平。

## Decision

1. **金額一律以 `int64` 儲存與傳遞，單位為該幣別的最小貨幣單位（minor unit）**：TWD 100 → `100`、USD 1.00 → `100`、KWD 1.000 → `1000`、JPY 100 → `100`。
2. **幣別為 ISO 4217 大寫三碼字串**，與金額永遠成對出現。`pkg/money.Money{Amount int64; Currency Currency}` 是唯一合法的金額型別。
3. **禁止對金額做裸 `int64` 運算**：所有加減乘除、比較、分攤都透過 `pkg/money` 的方法；`golangci-lint` 自訂規則（`forbidigo` + 欄位命名 `*_amount` 的 AST 檢查）阻擋 `payments.Amount + x` 這類寫法。
4. `pkg/money` 提供：
   - `New(amount int64, currency string) (Money, error)`：驗證幣別存在且支援。
   - `Add / Sub / Cmp / IsZero / IsNegative`：**幣別不同時回傳錯誤**，不做隱性換算。
   - `Allocate(ratios []int) []Money`：分攤時採「最大餘數法」，保證總和等於原金額（帳本平衡的前提）。
   - `MulRatio(num, den int64, mode RoundingMode)`：手續費計算（例如 2.9% + 30），捨入模式明確指定（`HalfEven` 為預設，`Up`/`Down` 依 PSP 合約）。
   - `Exponent(currency) int`：小數位數（0 / 2 / 3），僅用於**顯示與解析**，不用於儲存。
   - `ParseDecimal("12.34", "USD")` / `FormatDecimal()`：只在系統邊界（API 文件範例、報表、結算檔解析）使用。
5. **API（REST / gRPC / 事件）皆以整數 minor unit 傳遞**，JSON 欄位 `amount` 為 integer（不是字串、不是小數）。OpenAPI 標示 `type: integer, format: int64, minimum: 1`。
6. **DB 欄位型別為 `BIGINT`**，配合 `CHECK (amount >= 0)`（或業務允許的範圍）；**禁止** `NUMERIC`、`DECIMAL`、`MONEY`、`REAL`、`DOUBLE PRECISION`。
7. **上限**：單筆金額 ≤ `9_007_199_254_740_991`（2^53 − 1）以保證 JavaScript 商戶端能無損解析；`pkg/money` 在 `New` 時檢查。
8. **溢位**：`Add` / `MulRatio` 使用溢位檢查（`math/bits` 或 `(a > 0 && b > MaxInt64 - a)`），溢位回傳錯誤而非 wrap。
9. **不做自動換匯**（v1 非目標）；任何跨幣別操作在 `pkg/money` 層即被拒絕。

## Consequences

### 正面

- 運算精確、可比較、可做等號判斷；帳本「借貸總額相等」可用整數相等直接驗證。
- DB 聚合（`SUM(amount)`）精確且快速；`BIGINT` 索引效率高。
- 與主流 PSP（Stripe、Adyen）的 API 慣例一致（它們也用 minor unit 整數），adapter 不需轉換。
- 序列化無歧義：protobuf `int64`、JSON integer、SQL `BIGINT` 一一對應。

### 負面 / 代價

- **幣別小數位數的知識必須存在於 `pkg/money`**（ISO 4217 表），新增幣別需更新套件並發版；以 `currencies.go` 表格化並附測試。
- 顯示層（商戶後台、報表）必須自己除以 `10^exponent`，容易有人寫錯；`FormatDecimal` 是唯一合法途徑。
- **JSON 的 int64 在 JavaScript 超過 2^53 會失真**，因此設上限；這對支付金額而言綽綽有餘（USD 90 兆）。
- 部分 PSP（例如某些本地 PSP）API 用小數字串，adapter 需在邊界以 `ParseDecimal` 轉換，並以 exponent 驗證不會截斷（例如 PSP 回 `"12.345"` 給 USD 應視為錯誤）。
- 三位小數幣別（KWD）的 `Allocate` 與捨入測試需特別覆蓋（見 `09` §5）。

## Alternatives considered

| 選項 | 為何不選 |
|---|---|
| `float64` | 不精確，直接排除。 |
| `decimal` 套件（`shopspring/decimal`）以「元」為單位 | 精確，但每個值攜帶 scale、運算慢 10–50 倍、DB 需 `NUMERIC`、JSON 需字串、與 PSP API 的整數慣例不一致，且仍需知道幣別 exponent 才能正確捨入。複雜度高而收益低。 |
| `int64` 但單位為「元 × 10^6」（固定 micro 單位，Google Ads 做法） | 統一 scale 方便跨幣別比較，但會出現不可能的金額（TWD 0.000001），每次對外都要轉換，且與 PSP 慣例不一致。 |
| 字串金額（`"12.34"`）在 API 傳遞 | 避免 JS 精度問題，但每個客戶端都要自己解析與驗證格式；integer + 2^53 上限更簡單。 |
| `uint64` | 退款、沖銷分錄、負餘額（chargeback 後）都需要負數表示；`int64` 更自然。 |

## 對 01 文件的影響

無需修改 01：01 §5.1 已定義 Money；本 ADR 補充 2^53−1 上限、捨入模式與 `Allocate` 規則，建議日後併入 `02` §0.1。

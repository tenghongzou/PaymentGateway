# 結算檔測試樣本

- `mock-2026-08-19.csv`：provider-mock 格式（`type,provider_reference,amount_minor,currency,fee_minor,settled_at`），
  金額為最小貨幣單位整數，`settled_at` 為 RFC 3339 UTC。供 `internal/reconciliation` 的單元 / 整合測試與本機 `ImportSettlementFile` 試跑使用。
- `stripe-balance-sample.csv`：Stripe Balance Transactions 報表的欄位子集（金額為主單位小數），供 stripe parser 測試。

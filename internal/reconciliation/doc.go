// Package reconciliation 為 reconciliation-service 的程式碼根目錄（匯入 PSP 結算檔、與帳本/付款讀模型比對、差異報表）。
//
// 三層結構（docs/01 §7）：
//
//	internal/reconciliation/
//	├── domain/            SettlementFile / SettlementLine、Parser（mock CSV、Stripe balance CSV）、
//	│                      PaymentRecord 讀模型、Matcher（五種 discrepancy kind）、Run 聚合、Discrepancy 狀態機
//	├── app/               use cases（ImportSettlementFile、Get/ListReconciliationRuns、ListDiscrepancies、
//	│                      ResolveDiscrepancy、HandlePaymentEvent）與 ports；porttest/ 為 in-memory fake
//	└── adapter/
//	    ├── grpc/          pg.reconciliation.v1.ReconciliationService（5 rpc；source_url 支援 file:// 與 http(s)，上限 50MB）
//	    ├── postgres/      repository 實作（migrations/reconciliation 為 schema 事實來源；integration build tag 測試）
//	    └── kafka/         payment.events consumer（group reconciliation-service）
//
// 事件：比對完成後以 JSON payload 寫入 outbox（topic reconciliation.events）：
// reconciliation.run.completed、每筆對上的付款一則 settlement.posted（ledger J-STL）、reconciliation.discrepancy.resolved。
//
// 待辦（TODO）：
//   - migrations：discrepancies.kind CHECK 加入 'fee_mismatch'；payment_records 加 fee 欄位（目前 fee 不持久化）
//   - outbox payload 改 protobuf ReconciliationEvent；s3:// 來源；Stripe parser 完整欄位；自動 re-match（docs/05 §9.2 第 5 點）
package reconciliation

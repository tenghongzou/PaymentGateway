// Package ledger 為 ledger-service 的程式碼根目錄（雙式記帳：帳戶、日記帳、分錄、餘額；消費 payment.events 記帳）。
//
// 三層結構（docs/01 §7）：
//
//	internal/ledger/
//	├── domain/   科目表（10 科目）、Account / Journal / Entry、FeePolicy、分錄範本 TemplateFor、Reverse、Balances 恆等式
//	├── app/      use cases（PostJournal / HandlePaymentEvent / 查詢）與 ports（TxRunner、AccountRepo、JournalRepo、BalanceRepo、Inbox、OutboxStore、Clock）
//	└── adapter/
//	    ├── grpc/      pg.ledger.v1.LedgerService 7 個 RPC
//	    ├── postgres/  repository 實作（migrations/ledger 為 schema 事實來源；append-only，只有 INSERT / SELECT）
//	    └── kafka/     payment.events consumer（pkg/eventbus）；outbox relay 於 cmd/ledger-service 組裝
//
// 事件 → 分錄對照（docs/02 §7.3）見 domain/template.go；test-mode 事件記到 code 前綴 "test:" 的獨立帳戶。
package ledger

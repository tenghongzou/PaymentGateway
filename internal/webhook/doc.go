// Package webhook 為 webhook-service 的程式碼根目錄：消費 payment.events → 對商戶的 webhook 投遞
// （HMAC-SHA256 簽章、指數退避 10 次、死信、410 停用、SSRF 防護）。
//
// 結構（docs/01 §7）：
//
//	internal/webhook/
//	├── domain/    Event（PaymentEvent → OpenAPI Event JSON）、Delivery 狀態機與退避時程、Endpoint 訂閱過濾、Signer、SSRF URLPolicy
//	├── app/       ports（Transactor / Inbox / EventRepo / DeliveryRepo / EndpointSource / HTTPSender / Clock）、
//	│              use cases（IngestEvent / DispatchDue / ReapStuckInFlight / RetryDelivery / 查詢）、EndpointCache、workers
//	│   └── apptest/  記憶體版 ports（測試用）
//	├── adapter/
//	│   ├── grpc/      pg.webhook.v1.WebhookService 實作 + merchant-service client（端點 / secret / 停用）
//	│   ├── postgres/  repository 實作（取件 UPDATE ... FOR UPDATE SKIP LOCKED RETURNING；//go:build integration 測試）
//	│   ├── http/      HTTPSender（DialContext 層 SSRF 檢查、不跟隨 redirect、body 截 4KB）
//	│   └── kafka/     payment.events consumer（group webhook-service）
//	└── devsink/   本機 webhook 接收端（`webhook-service sink`），印出並驗簽
//
// 組裝在 cmd/webhook-service/main.go。
package webhook

// Package providerstripe 為 provider-stripe 的程式碼根目錄（Stripe adapter，實作 pg.provider.v1.ProviderAdapter）。
//
// 預期結構（adapter 無 DB，不需 domain / postgres 層）：
//
//	internal/providerstripe/
//	├── adapter/grpc/   ProviderAdapter gRPC 實作（目前 Unimplemented 骨架，HealthCheck 回 NOT_SERVING）
//	├── mapping.go      Stripe decline_code / HTTP 狀態 → ProviderErrorCategory（docs/02 §11.1 對照表）
//	└── client.go       Stripe REST client（PG_STRIPE_API_KEY / PG_STRIPE_API_BASE_URL；Idempotency-Key header）
//
// 待辦：Authorize（PaymentIntent confirm）、Capture、Void（cancel）、Refund、GetPaymentStatus、
// ParseWebhook（Stripe-Signature 驗簽 → ProviderWebhookEvent）、HealthCheck（capabilities）。
package providerstripe

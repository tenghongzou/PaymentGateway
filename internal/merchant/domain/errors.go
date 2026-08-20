// Package domain 為 merchant-service 的領域層：商戶、API Key、簽章 secret、Webhook 端點、路由偏好與領域錯誤。
//
// 只依賴標準庫與 pkg/（apperr、ids），不得 import app / adapter（docs/01 §7 import 規則）。
package domain

import "github.com/tenghongzou/paymentgateway/pkg/apperr"

// 領域錯誤（type / code 對齊 docs/02 §10 與 docs/03 §7；HTTP / gRPC 對應由 pkg/apperr、pkg/grpcx 決定）。
var (
	// ErrNotFound 表示資源不存在或不屬於此商戶（404）。
	ErrNotFound = apperr.ErrResourceMissing
	// ErrParameterMissing 表示缺必填欄位（400，param 指出欄位）。
	ErrParameterMissing = apperr.ErrParameterMissing
	// ErrParameterInvalid 表示欄位格式 / 值錯誤（400，param 指出欄位）。
	ErrParameterInvalid = apperr.ErrParameterInvalid
	// ErrConcurrentModify 表示樂觀鎖衝突（409）。
	ErrConcurrentModify = apperr.ErrConcurrentModify

	// ErrInvalidAPIKey：key 不存在 / 格式錯誤（401）。
	ErrInvalidAPIKey = apperr.New(apperr.TypeAuthentication, "invalid_api_key", "Invalid API key.")
	// ErrAPIKeyRevoked：key 已撤銷（401）。
	ErrAPIKeyRevoked = apperr.New(apperr.TypeAuthentication, "api_key_revoked", "This API key has been revoked.")
	// ErrAPIKeyExpired：key 已過期（401）。
	ErrAPIKeyExpired = apperr.New(apperr.TypeAuthentication, "api_key_expired", "This API key has expired.")
	// ErrMerchantSuspended：商戶暫停，不允許此操作（403）。
	ErrMerchantSuspended = apperr.New(apperr.TypeAuthentication, "merchant_suspended", "This merchant is suspended.")
	// ErrMerchantClosed：商戶已關閉，所有寫入操作拒絕（403）。
	ErrMerchantClosed = apperr.New(apperr.TypeAuthentication, "merchant_closed", "This merchant is closed.")
	// ErrInsufficientPermissions：key scope 不足（403）。
	ErrInsufficientPermissions = apperr.New(apperr.TypeAuthentication, "insufficient_permissions", "This API key does not have the required scope.")
	// ErrLivemodeMismatch：test key 操作 live 資源或反之（403）。
	ErrLivemodeMismatch = apperr.New(apperr.TypeAuthentication, "livemode_mismatch", "The API key mode does not match the resource mode.")

	// ErrInvalidStateTransition：狀態機不允許此轉移（409）。
	ErrInvalidStateTransition = apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", "The current status does not allow this transition.")
	// ErrAlreadyExists：唯一鍵衝突（external_ref 重複）。
	// TODO(pkg/apperr)：codeStatus 尚無 resource_already_exists，目前會落到 400 / INVALID_ARGUMENT；建議補 409。
	ErrAlreadyExists = apperr.New(apperr.TypeInvalidRequest, "resource_already_exists", "A resource with the same unique reference already exists.")
	// ErrWebhookURLInvalid：非 https、私有網段或無法解析（422）。
	ErrWebhookURLInvalid = apperr.New(apperr.TypeInvalidRequest, "webhook_url_invalid", "The webhook URL is invalid.")
	// ErrWebhookEndpointLimit：超過端點上限（422）。
	ErrWebhookEndpointLimit = apperr.New(apperr.TypeInvalidRequest, "webhook_endpoint_limit_reached", "The maximum number of webhook endpoints has been reached.")
	// ErrAPIKeyLimit：超過有效 key 上限（422）。
	ErrAPIKeyLimit = apperr.New(apperr.TypeInvalidRequest, "api_key_limit_reached", "The maximum number of active API keys for this mode has been reached.")
	// ErrMetadataTooLarge：metadata 超過限制（400）。
	ErrMetadataTooLarge = apperr.New(apperr.TypeInvalidRequest, "metadata_too_large", "Metadata exceeds the allowed size.")
	// ErrSecretUnavailable：signing secret 無法解密（KEK 缺失或密文損毀）。
	ErrSecretUnavailable = apperr.New(apperr.TypeAPI, "internal_error", "Signing secret could not be decrypted.")
)

// invalid 產生帶 param 與訊息的 ErrParameterInvalid。
func invalid(param, format string, args ...any) error {
	return ErrParameterInvalid.WithParam(param).WithMessage(format, args...)
}

// missing 產生帶 param 的 ErrParameterMissing。
func missing(param string) error {
	return ErrParameterMissing.WithParam(param).WithMessage("%s is required.", param)
}

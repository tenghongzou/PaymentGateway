package domain

// ProviderErrorCategory 為標準化供應商錯誤分類（docs/02 §11）。
type ProviderErrorCategory string

// 錯誤分類列舉。
const (
	CategoryNone                   ProviderErrorCategory = ""
	CategoryDeclinedHard           ProviderErrorCategory = "declined_hard"
	CategoryDeclinedSoft           ProviderErrorCategory = "declined_soft"
	CategoryFraudSuspected         ProviderErrorCategory = "fraud_suspected"
	CategoryAuthenticationRequired ProviderErrorCategory = "authentication_required"
	CategoryAuthenticationFailed   ProviderErrorCategory = "authentication_failed"
	CategoryInvalidRequest         ProviderErrorCategory = "invalid_request"
	CategoryProviderConfigError    ProviderErrorCategory = "provider_config_error"
	CategoryProviderUnavailable    ProviderErrorCategory = "provider_unavailable"
	CategoryProviderRateLimited    ProviderErrorCategory = "provider_rate_limited"
	CategoryProviderTimeout        ProviderErrorCategory = "provider_timeout"
	CategoryDuplicateRequest       ProviderErrorCategory = "duplicate_request"
	CategoryUnsupportedOperation   ProviderErrorCategory = "unsupported_operation"
	CategoryUnknown                ProviderErrorCategory = "unknown"
)

// categoryRule 描述每個分類的決策屬性（docs/02 §11 表）。
type categoryRule struct {
	retrySameProvider bool          // 同 Provider 自動重試
	canFailover       bool          // 可 failover
	attemptStatus     AttemptStatus // 對應 Attempt 狀態（§11.0）
	restCode          string        // 對外 REST code
}

var categoryRules = map[ProviderErrorCategory]categoryRule{ //nolint:exhaustive // CategoryNone 代表「無錯誤」，刻意不列
	CategoryDeclinedHard:           {false, false, AttemptDeclined, "card_declined"},
	CategoryDeclinedSoft:           {true, true, AttemptDeclined, "card_declined"},
	CategoryFraudSuspected:         {false, false, AttemptDeclined, "card_declined"},
	CategoryAuthenticationRequired: {false, false, AttemptRequiresAction, "authentication_required"},
	CategoryAuthenticationFailed:   {false, false, AttemptDeclined, "card_declined"},
	CategoryInvalidRequest:         {false, false, AttemptDeclined, "provider_rejected"},
	CategoryProviderConfigError:    {false, true, AttemptUnavailable, "provider_unavailable"},
	CategoryProviderUnavailable:    {false, true, AttemptUnavailable, "provider_unavailable"},
	CategoryProviderRateLimited:    {true, true, AttemptUnavailable, "provider_unavailable"},
	CategoryProviderTimeout:        {false, false, AttemptUnknown, "provider_timeout"},
	CategoryDuplicateRequest:       {false, false, AttemptDeclined, "provider_rejected"},
	CategoryUnsupportedOperation:   {false, false, AttemptDeclined, "provider_rejected"},
	CategoryUnknown:                {false, false, AttemptUnknown, "provider_timeout"},
}

// IsRetryable 回傳是否可在同一 Provider 自動重試（docs/02 §11「同 Provider 自動重試」欄）。
func (c ProviderErrorCategory) IsRetryable() bool { return categoryRules[c].retrySameProvider }

// CanFailover 回傳此分類是否允許切換 Provider（declined_soft 還需 decline code 在白名單，見 CanFailoverDecline）。
func (c ProviderErrorCategory) CanFailover() bool { return categoryRules[c].canFailover }

// AttemptStatus 回傳分類對應的 Attempt 狀態（docs/02 §11.0）。
func (c ProviderErrorCategory) AttemptStatus() AttemptStatus {
	if r, ok := categoryRules[c]; ok {
		return r.attemptStatus
	}
	return AttemptUnknown
}

// RESTCode 回傳對外的 provider_error code。
func (c ProviderErrorCategory) RESTCode() string {
	if r, ok := categoryRules[c]; ok {
		return r.restCode
	}
	return "provider_timeout"
}

// IsValid 檢查分類是否為已知值。
func (c ProviderErrorCategory) IsValid() bool {
	_, ok := categoryRules[c]
	return ok
}

// softDeclineFailoverWhitelist 為 declined_soft 允許 failover 的正規化 decline code（docs/02 §9.5）。
var softDeclineFailoverWhitelist = map[string]bool{
	"processing_error":   true,
	"issuer_unavailable": true,
	"try_again_later":    true,
}

// CanFailoverDecline 綜合分類與 decline code 判斷是否可 failover。
func CanFailoverDecline(c ProviderErrorCategory, declineCode string) bool {
	if !c.CanFailover() {
		return false
	}
	if c == CategoryDeclinedSoft {
		return softDeclineFailoverWhitelist[declineCode]
	}
	return true
}

// retryableDeclineCodes 為 failure.retryable 的對商戶語意（docs/02 §3.4）。
var retryableDeclineCodes = map[string]bool{
	"insufficient_funds": true, "try_again_later": true, "processing_error": true, "issuer_unavailable": true,
	"authentication_failed": true, "velocity_exceeded": true,
}

// IsRetryableDecline 回傳「同一付款人稍後重試是否有意義」。
func IsRetryableDecline(c ProviderErrorCategory, declineCode string) bool {
	switch c {
	case CategoryProviderUnavailable, CategoryProviderRateLimited, CategoryProviderTimeout, CategoryProviderConfigError, CategoryUnknown:
		return true
	case CategoryFraudSuspected:
		return false
	case CategoryDeclinedHard, CategoryDeclinedSoft, CategoryAuthenticationFailed, CategoryAuthenticationRequired,
		CategoryInvalidRequest, CategoryDuplicateRequest, CategoryUnsupportedOperation, CategoryNone:
		return retryableDeclineCodes[declineCode]
	default:
		return retryableDeclineCodes[declineCode]
	}
}

// Failure 為寫入 payments.failure_* 的值物件。
type Failure struct {
	Category  ProviderErrorCategory
	Code      string // 正規化 decline_code / 錯誤碼
	Message   string // 商戶可見訊息（已遮罩）
	Provider  string
	Retryable bool
}

// PublicCode 回傳對外顯示的 decline code：fraud_suspected 一律對外 generic_decline（docs/02 §3.4）。
func (f Failure) PublicCode() string {
	if f.Category == CategoryFraudSuspected {
		return "generic_decline"
	}
	if f.Code == "" {
		return f.Category.RESTCode()
	}
	return f.Code
}

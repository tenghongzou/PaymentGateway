package providermock

import (
	"strconv"
	"strings"
	"time"

	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
)

// Behavior 描述 Authorize 時的行為。
type Behavior int

// 行為列舉。
const (
	BehaveApprove Behavior = iota
	BehaveDecline
	BehaveRequiresAction
	BehaveTimeout
	BehaveUnavailable
	BehaveUnavailableOnce
	BehaveRateLimited
	BehaveInvalid
)

// Scenario 為 token 對應的情境。
type Scenario struct {
	Behavior      Behavior
	Category      providerv1.ProviderErrorCategory
	Code          string
	Message       string
	Retryable     bool
	Latency       time.Duration
	AuthValidity  time.Duration
	CaptureFails  bool
	RefundFails   bool
	CaptureTimout bool
}

// 支援的 token（docs/09 §3.1；未列出的 token 視同 tok_ok）。
const (
	TokOK                = "tok_ok"
	TokDeclineHard       = "tok_decline_hard"
	TokDeclineSoft       = "tok_decline_soft"
	TokInsufficientFunds = "tok_insufficient_funds"
	TokFraud             = "tok_fraud"
	TokInvalid           = "tok_invalid"
	Tok3DS               = "tok_3ds"
	TokTimeout           = "tok_timeout"
	TokUnavailable       = "tok_unavailable"
	TokUnavailableOnce   = "tok_unavailable_once"
	TokRateLimited       = "tok_rate_limited"
	TokCaptureFail       = "tok_capture_fail"
	TokRefundFail        = "tok_refund_fail"
	TokShortAuth         = "tok_short_auth"
	TokSlowPrefix        = "tok_slow_"
)

// ScenarioFor 依 token 回傳情境；token 前綴 tok_ 之後的行為是確定性的。
func ScenarioFor(token string) Scenario {
	switch token {
	case TokDeclineHard:
		return Scenario{Behavior: BehaveDecline, Category: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD, Code: "stolen_card", Message: "Your card was declined."}
	case TokDeclineSoft:
		return Scenario{Behavior: BehaveDecline, Category: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_SOFT, Code: "try_again_later", Message: "The issuer is temporarily unavailable; try again later.", Retryable: true}
	case TokInsufficientFunds:
		return Scenario{Behavior: BehaveDecline, Category: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD, Code: "insufficient_funds", Message: "The card has insufficient funds.", Retryable: true}
	case TokFraud:
		return Scenario{Behavior: BehaveDecline, Category: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_FRAUD_SUSPECTED, Code: "fraudulent", Message: "The payment was blocked by risk controls."}
	case TokInvalid:
		return Scenario{Behavior: BehaveInvalid, Category: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST, Code: "currency_not_supported", Message: "Currency not supported by this account."}
	case Tok3DS:
		return Scenario{Behavior: BehaveRequiresAction}
	case TokTimeout:
		return Scenario{Behavior: BehaveTimeout}
	case TokUnavailable:
		return Scenario{Behavior: BehaveUnavailable, Category: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_PROVIDER_UNAVAILABLE, Code: "service_unavailable", Message: "Provider is unavailable.", Retryable: true}
	case TokUnavailableOnce:
		return Scenario{Behavior: BehaveUnavailableOnce, Category: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_PROVIDER_UNAVAILABLE, Code: "service_unavailable", Message: "Provider is unavailable (first attempt).", Retryable: true}
	case TokRateLimited:
		return Scenario{Behavior: BehaveRateLimited, Category: providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_PROVIDER_UNAVAILABLE, Code: "rate_limited", Message: "Too many requests.", Retryable: true}
	case TokCaptureFail:
		return Scenario{Behavior: BehaveApprove, CaptureFails: true}
	case TokRefundFail:
		return Scenario{Behavior: BehaveApprove, RefundFails: true}
	case TokShortAuth:
		return Scenario{Behavior: BehaveApprove, AuthValidity: 90 * time.Minute}
	}
	if strings.HasPrefix(token, TokSlowPrefix) {
		if ms, err := strconv.Atoi(strings.TrimPrefix(token, TokSlowPrefix)); err == nil && ms > 0 {
			return Scenario{Behavior: BehaveApprove, Latency: time.Duration(ms) * time.Millisecond}
		}
	}
	return Scenario{Behavior: BehaveApprove}
}

package domain

import (
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
)

// 領域錯誤（type / code 依 docs/03 §7 與 docs/02 §10）。
var (
	ErrInvalidTransition       = apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", "The payment is not in a state that allows this operation.")
	ErrAmountExceedsAuthorized = apperr.New(apperr.TypeInvalidRequest, "capture_amount_exceeds_authorized", "Capture amount exceeds the authorized amount.").WithParam("amount")
	ErrRefundExceedsAvailable  = apperr.New(apperr.TypeInvalidRequest, "refund_amount_exceeds_captured", "Refund amount exceeds the refundable amount.").WithParam("amount")
	ErrCurrencyMismatch        = apperr.New(apperr.TypeInvalidRequest, "currency_mismatch", "Currency does not match the payment currency.").WithParam("amount.currency")
	ErrAmountTooSmall          = apperr.New(apperr.TypeInvalidRequest, "amount_too_small", "Amount must be at least 1 minor unit.").WithParam("amount")
	ErrInvalidCurrency         = apperr.New(apperr.TypeInvalidRequest, "invalid_currency", "Currency must be a supported ISO 4217 code.").WithParam("amount.currency")
	ErrPaymentNotRefundable    = apperr.New(apperr.TypeInvalidRequest, "payment_not_refundable", "The payment is not in a refundable state.")
	ErrPaymentDisputed         = apperr.New(apperr.TypeInvalidRequest, "payment_disputed", "The payment has an open dispute.")
	ErrPaymentExpired          = apperr.New(apperr.TypeInvalidRequest, "payment_expired", "The authorization or action window has expired.")
	ErrVoidNotAllowed          = apperr.New(apperr.TypeInvalidRequest, "void_not_allowed", "The payment has been captured; create a refund instead.")
	ErrOperationInProgress     = apperr.New(apperr.TypeInvalidRequest, "operation_in_progress", "Another operation on this payment is in progress.")
	ErrNoRouteAvailable        = apperr.New(apperr.TypeInvalidRequest, "no_route_available", "No payment provider can process this payment.")
	ErrPaymentMethodInvalid    = apperr.New(apperr.TypeInvalidRequest, "payment_method_invalid", "The payment method is invalid.").WithParam("payment_method")
	ErrPaymentMethodMissing    = apperr.New(apperr.TypeInvalidRequest, "parameter_missing", "payment_method is required.").WithParam("payment_method")
	ErrPANNotAllowed           = apperr.New(apperr.TypeInvalidRequest, "pan_not_allowed", "Raw card numbers are not accepted; use a provider token.").WithParam("payment_method.card.token")
	ErrMetadataTooLarge        = apperr.New(apperr.TypeInvalidRequest, "metadata_too_large", "metadata may contain at most 50 keys; values at most 500 characters.").WithParam("metadata")
	ErrPaymentNotFound         = apperr.New(apperr.TypeInvalidRequest, "resource_missing", "No such payment.").WithParam("id")
	ErrRefundNotFound          = apperr.New(apperr.TypeInvalidRequest, "resource_missing", "No such refund.").WithParam("id")
	ErrIdempotencyKeyMismatch  = apperr.ErrIdempotencyMismatch
	ErrProviderUnavailable     = apperr.New(apperr.TypeProvider, "provider_unavailable", "The payment provider is temporarily unavailable.")
	ErrProviderTimeout         = apperr.New(apperr.TypeProvider, "provider_timeout", "The payment provider did not respond in time; the outcome is unknown.")
	ErrProviderRejected        = apperr.New(apperr.TypeProvider, "provider_rejected", "The payment provider rejected the request.")
	ErrCardDeclined            = apperr.New(apperr.TypeProvider, "card_declined", "The card was declined.")
	ErrAuthenticationFailed    = apperr.New(apperr.TypeProvider, "authentication_failed", "Customer authentication failed.")
	ErrConcurrentModification  = apperr.ErrConcurrentModify
)

// TransitionError 建立帶目前狀態資訊的 ErrInvalidTransition。
func TransitionError(from, to Status) *apperr.Error {
	return ErrInvalidTransition.WithMessage("cannot transition payment from %q to %q", from, to)
}

// ProviderError 依 ProviderErrorCategory 建立對外的 provider_error（用於對既有資源的操作被 PSP 拒絕：capture / void / refund）。
func ProviderError(cat ProviderErrorCategory, code, message string) *apperr.Error {
	if message == "" {
		message = "The payment provider rejected the operation."
	}
	e := apperr.New(apperr.TypeProvider, cat.RESTCode(), message)
	if code != "" && (cat == CategoryDeclinedHard || cat == CategoryDeclinedSoft || cat == CategoryFraudSuspected) {
		// 對既有資源的拒絕把正規化 decline code 放進 message 尾端供商戶除錯（結構化欄位由 gateway 另行處理）。
		e = e.WithMessage("%s (decline_code=%s)", message, code)
	}
	return e
}

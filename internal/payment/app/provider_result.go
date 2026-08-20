package app

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// outcomeKind 為一次 Authorize 的歸類結果。
type outcomeKind int

const (
	outcomeApproved outcomeKind = iota
	outcomeRequiresAction
	outcomeFailed
)

// authOutcome 為 Authorize 回應（或傳輸錯誤）正規化後的結果。
type authOutcome struct {
	kind       outcomeKind
	category   domain.ProviderErrorCategory
	code       string
	message    string
	ref        string
	captured   bool
	amount     money.Money // 實際授權 / 請款金額（空則用 payment 金額）
	fee        int64
	authExpiry time.Time
	details    *domain.PaymentMethodDetails
	nextAction *domain.NextAction
}

// classifyTransportError 把 gRPC 傳輸層錯誤映射為 ProviderErrorCategory（docs/02 §11）。
func classifyTransportError(err error) (domain.ProviderErrorCategory, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.CategoryProviderTimeout, "deadline_exceeded"
	}
	st, ok := status.FromError(err)
	if !ok {
		return domain.CategoryUnknown, "transport_error"
	}
	switch st.Code() {
	case codes.DeadlineExceeded, codes.Canceled:
		return domain.CategoryProviderTimeout, st.Code().String()
	case codes.Unavailable, codes.Internal, codes.Aborted:
		return domain.CategoryProviderUnavailable, st.Code().String()
	case codes.ResourceExhausted:
		return domain.CategoryProviderRateLimited, st.Code().String()
	case codes.Unauthenticated, codes.PermissionDenied:
		return domain.CategoryProviderConfigError, st.Code().String()
	case codes.Unimplemented, codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange, codes.NotFound:
		return domain.CategoryInvalidRequest, st.Code().String()
	case codes.AlreadyExists:
		return domain.CategoryDuplicateRequest, st.Code().String()
	case codes.OK, codes.Unknown, codes.DataLoss:
		return domain.CategoryUnknown, st.Code().String()
	default:
		return domain.CategoryUnknown, st.Code().String()
	}
}

// categoryFromProto 把 adapter 回報的分類轉成領域分類；proto 的 PROVIDER_UNAVAILABLE 依 code 細分 rate limit。
func categoryFromProto(c providerv1.ProviderErrorCategory, code string) domain.ProviderErrorCategory {
	switch c {
	case providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD:
		return domain.CategoryDeclinedHard
	case providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_SOFT:
		return domain.CategoryDeclinedSoft
	case providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_PROVIDER_UNAVAILABLE:
		if code == "rate_limited" || code == "rate_limit_exceeded" {
			return domain.CategoryProviderRateLimited
		}
		return domain.CategoryProviderUnavailable
	case providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST:
		return domain.CategoryInvalidRequest
	case providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_FRAUD_SUSPECTED:
		return domain.CategoryFraudSuspected
	case providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_AUTHENTICATION_REQUIRED:
		return domain.CategoryAuthenticationRequired
	case providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_UNKNOWN, providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_UNSPECIFIED:
		return domain.CategoryUnknown
	default:
		return domain.CategoryUnknown
	}
}

// CategoryToProto 把領域分類轉回 proto（對外 API 用；細分類別收斂到 proto 的七種）。
func CategoryToProto(c domain.ProviderErrorCategory) providerv1.ProviderErrorCategory {
	switch c {
	case domain.CategoryDeclinedHard:
		return providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD
	case domain.CategoryDeclinedSoft:
		return providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_SOFT
	case domain.CategoryProviderUnavailable, domain.CategoryProviderRateLimited, domain.CategoryProviderConfigError:
		return providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_PROVIDER_UNAVAILABLE
	case domain.CategoryInvalidRequest, domain.CategoryUnsupportedOperation, domain.CategoryDuplicateRequest:
		return providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST
	case domain.CategoryFraudSuspected:
		return providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_FRAUD_SUSPECTED
	case domain.CategoryAuthenticationRequired, domain.CategoryAuthenticationFailed:
		return providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_AUTHENTICATION_REQUIRED
	case domain.CategoryProviderTimeout, domain.CategoryUnknown, domain.CategoryNone:
		return providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_UNKNOWN
	default:
		return providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_UNKNOWN
	}
}

// evaluateAuthorize 把 Authorize 的回應 / 錯誤正規化。
func evaluateAuthorize(resp *providerv1.AuthorizeResponse, err error, currency string) authOutcome {
	if err != nil {
		cat, code := classifyTransportError(err)
		return authOutcome{kind: outcomeFailed, category: cat, code: code, message: status.Convert(err).Message()}
	}
	res := resp.GetResult()
	if res == nil {
		return authOutcome{kind: outcomeFailed, category: domain.CategoryUnknown, code: "empty_response", message: "adapter returned no result"}
	}
	if !res.GetSuccess() {
		cat := categoryFromProto(res.GetErrorCategory(), res.GetProviderErrorCode())
		if cat == domain.CategoryAuthenticationRequired && resp.GetNextAction() != nil {
			return authOutcome{kind: outcomeRequiresAction, ref: res.GetProviderReference(), nextAction: nextActionFromProto(resp.GetNextAction())}
		}
		return authOutcome{kind: outcomeFailed, category: cat, code: res.GetProviderErrorCode(), message: res.GetProviderErrorMessage(), ref: res.GetProviderReference()}
	}
	out := authOutcome{ref: res.GetProviderReference(), fee: resp.GetFee().GetAmountMinor()}
	if resp.GetAuthorizationExpiresAt() != nil {
		out.authExpiry = resp.GetAuthorizationExpiresAt().AsTime()
	}
	out.details = detailsFromProto(resp.GetInstrumentDetails())
	switch resp.GetStatus() {
	case providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED:
		out.kind = outcomeApproved
		out.captured = true
		if m, err := money.FromProto(resp.GetCapturedAmount()); err == nil && m.Currency == currency && m.IsPositive() {
			out.amount = m
		}
	case providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_AUTHORIZED:
		out.kind = outcomeApproved
	case providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_REQUIRES_ACTION:
		out.kind = outcomeRequiresAction
		out.nextAction = nextActionFromProto(resp.GetNextAction())
	case providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_PENDING:
		// 非同步付款方式（銀行轉帳）：Phase 0 視同 requires_action（display）。
		out.kind = outcomeRequiresAction
		out.nextAction = nextActionFromProto(resp.GetNextAction())
	case providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED, providerv1.AuthorizationStatus_AUTHORIZATION_STATUS_UNSPECIFIED:
		out.kind = outcomeFailed
		out.category = domain.CategoryUnknown
		out.code = "inconsistent_response"
		out.message = "adapter reported success with a failed status"
	}
	return out
}

func nextActionFromProto(na *providerv1.NextAction) *domain.NextAction {
	if na == nil {
		return nil
	}
	out := &domain.NextAction{}
	if na.GetExpiresAt() != nil {
		out.ExpiresAt = na.GetExpiresAt().AsTime()
	}
	switch a := na.GetAction().(type) {
	case *providerv1.NextAction_Redirect:
		out.Type = "redirect"
		out.URL = a.Redirect.GetUrl()
		out.Method = a.Redirect.GetMethod()
		out.FormFields = a.Redirect.GetFormFields()
	case *providerv1.NextAction_ThreeDsChallenge:
		out.Type = "three_ds_challenge"
		out.ACSURL = a.ThreeDsChallenge.GetAcsUrl()
		out.CReq = a.ThreeDsChallenge.GetCreq()
		out.TxnID = a.ThreeDsChallenge.GetTransactionId()
		out.Version = a.ThreeDsChallenge.GetVersion()
	case *providerv1.NextAction_Display:
		out.Type = "display"
		out.Display = a.Display.GetDetails()
		if out.Display == nil {
			out.Display = map[string]string{}
		}
		out.Display["type"] = a.Display.GetType()
	default:
		out.Type = "redirect"
	}
	return out
}

func detailsFromProto(d *providerv1.InstrumentDetails) *domain.PaymentMethodDetails {
	if d == nil {
		return nil
	}
	out := &domain.PaymentMethodDetails{
		Brand: d.GetBrand(), Last4: d.GetLast4(), Issuer: d.GetIssuer(), IssuerCountry: d.GetIssuerCountry(),
		Funding: d.GetFunding(), ThreeDSResult: d.GetThreeDsResult(),
	}
	if d.GetWalletType() != providerv1.WalletType_WALLET_TYPE_UNSPECIFIED {
		out.WalletType = walletTypeString(d.GetWalletType())
	}
	return out
}

func walletTypeString(w providerv1.WalletType) string {
	switch w {
	case providerv1.WalletType_WALLET_TYPE_APPLE_PAY:
		return "apple_pay"
	case providerv1.WalletType_WALLET_TYPE_GOOGLE_PAY:
		return "google_pay"
	case providerv1.WalletType_WALLET_TYPE_LINE_PAY:
		return "line_pay"
	case providerv1.WalletType_WALLET_TYPE_UNSPECIFIED:
		return ""
	default:
		return ""
	}
}

// resultCategory 把非 Authorize 操作（capture/void/refund）的回應歸類；ok=true 表示成功。
func resultCategory(res *providerv1.ProviderResult, err error) (ok bool, cat domain.ProviderErrorCategory, code, msg string) {
	if err != nil {
		cat, code = classifyTransportError(err)
		return false, cat, code, status.Convert(err).Message()
	}
	if res == nil {
		return false, domain.CategoryUnknown, "empty_response", "adapter returned no result"
	}
	if res.GetSuccess() {
		return true, domain.CategoryNone, "", ""
	}
	return false, categoryFromProto(res.GetErrorCategory(), res.GetProviderErrorCode()), res.GetProviderErrorCode(), res.GetProviderErrorMessage()
}

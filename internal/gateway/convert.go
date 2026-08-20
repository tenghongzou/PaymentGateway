package gateway

import (
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// ---- JSON schemas（與 api/openapi/payment-gateway.yaml 一致）----

type moneyJSON struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

func (m *moneyJSON) toProto() *commonv1.Money {
	if m == nil {
		return nil
	}
	return &commonv1.Money{AmountMinor: m.AmountMinor, Currency: strings.ToUpper(m.Currency)}
}

func moneyFromProto(m *commonv1.Money) *moneyJSON {
	if m == nil {
		return nil
	}
	return &moneyJSON{AmountMinor: m.GetAmountMinor(), Currency: m.GetCurrency()}
}

type cardTokenJSON struct {
	Token         string `json:"token"`
	TokenProvider string `json:"token_provider"`
	Brand         string `json:"brand,omitempty"`
	Last4         string `json:"last4,omitempty"`
	ExpMonth      int32  `json:"exp_month,omitempty"`
	ExpYear       int32  `json:"exp_year,omitempty"`
	HolderName    string `json:"holder_name,omitempty"`
}

type walletJSON struct {
	Type             string `json:"type"`
	EncryptedPayload string `json:"encrypted_payload,omitempty"`
}

type bankTransferJSON struct {
	Country   string `json:"country"`
	BankCode  string `json:"bank_code,omitempty"`
	PayerName string `json:"payer_name,omitempty"`
}

type paymentMethodInputJSON struct {
	Type         string            `json:"type"`
	Card         *cardTokenJSON    `json:"card,omitempty"`
	Wallet       *walletJSON       `json:"wallet,omitempty"`
	BankTransfer *bankTransferJSON `json:"bank_transfer,omitempty"`
}

type customerJSON struct {
	ID                string `json:"id,omitempty"`
	Email             string `json:"email,omitempty"`
	Name              string `json:"name,omitempty"`
	Phone             string `json:"phone,omitempty"`
	IPAddress         string `json:"ip_address,omitempty"`
	UserAgent         string `json:"user_agent,omitempty"`
	BillingCountry    string `json:"billing_country,omitempty"`
	BillingPostalCode string `json:"billing_postal_code,omitempty"`
}

type createPaymentJSON struct {
	Amount              *moneyJSON              `json:"amount"`
	CaptureMethod       string                  `json:"capture_method"`
	PaymentMethod       *paymentMethodInputJSON `json:"payment_method"`
	Customer            *customerJSON           `json:"customer"`
	Metadata            map[string]string       `json:"metadata"`
	Description         string                  `json:"description"`
	ReturnURL           string                  `json:"return_url"`
	StatementDescriptor string                  `json:"statement_descriptor"`
	ThreeDS             string                  `json:"three_ds"`
}

// panLike 偵測 13–19 位純數字（疑似卡號）。
var panLike = regexp.MustCompile(`^\d{13,19}$`)

func (in *createPaymentJSON) toProto(p *Principal, idemKey string) (*paymentv1.CreatePaymentRequest, error) {
	if in.Amount == nil {
		return nil, apperr.ErrParameterMissing.WithMessage("amount is required").WithParam("amount")
	}
	if _, err := money.New(in.Amount.AmountMinor, strings.ToUpper(in.Amount.Currency)); err != nil {
		return nil, apperr.New(apperr.TypeInvalidRequest, "currency_not_supported", "amount.currency must be a supported ISO 4217 code and amount_minor must be >= 0").WithParam("amount")
	}
	if in.PaymentMethod == nil {
		return nil, apperr.ErrParameterMissing.WithMessage("payment_method is required").WithParam("payment_method")
	}
	req := &paymentv1.CreatePaymentRequest{
		IdempotencyKey: idemKey, MerchantId: p.MerchantID, Amount: in.Amount.toProto(),
		Metadata: in.Metadata, Description: in.Description, ReturnUrl: in.ReturnURL, StatementDescriptor: in.StatementDescriptor, Livemode: p.LiveMode,
	}
	switch in.CaptureMethod {
	case "", "automatic":
		req.CaptureMethod = paymentv1.CaptureMethod_CAPTURE_METHOD_AUTOMATIC
	case "manual":
		req.CaptureMethod = paymentv1.CaptureMethod_CAPTURE_METHOD_MANUAL
	default:
		return nil, apperr.ErrParameterInvalid.WithMessage("capture_method must be automatic or manual").WithParam("capture_method")
	}
	switch in.ThreeDS {
	case "", "automatic":
		req.ThreeDs = providerv1.ThreeDsPreference_THREE_DS_PREFERENCE_UNSPECIFIED
	case "required":
		req.ThreeDs = providerv1.ThreeDsPreference_THREE_DS_PREFERENCE_REQUIRED
	case "avoid":
		req.ThreeDs = providerv1.ThreeDsPreference_THREE_DS_PREFERENCE_AVOID
	default:
		return nil, apperr.ErrParameterInvalid.WithMessage("three_ds must be automatic, required or avoid").WithParam("three_ds")
	}
	pm := in.PaymentMethod
	switch pm.Type {
	case "card":
		if pm.Card == nil || pm.Card.Token == "" {
			return nil, apperr.ErrParameterMissing.WithMessage("payment_method.card.token is required").WithParam("payment_method.card.token")
		}
		if panLike.MatchString(pm.Card.Token) {
			return nil, apperr.New(apperr.TypeInvalidRequest, "pan_not_allowed", "Raw card numbers are not accepted; use a provider token.").WithParam("payment_method.card.token")
		}
		if pm.Card.TokenProvider == "" {
			return nil, apperr.ErrParameterMissing.WithMessage("payment_method.card.token_provider is required").WithParam("payment_method.card.token_provider")
		}
		req.PaymentMethod = &paymentv1.PaymentMethod{Method: &paymentv1.PaymentMethod_CardToken{CardToken: &paymentv1.CardToken{
			Token: pm.Card.Token, TokenProvider: pm.Card.TokenProvider, Brand: pm.Card.Brand, Last4: pm.Card.Last4,
			ExpMonth: pm.Card.ExpMonth, ExpYear: pm.Card.ExpYear, HolderName: pm.Card.HolderName,
		}}}
	case "wallet":
		if pm.Wallet == nil || pm.Wallet.Type == "" {
			return nil, apperr.ErrParameterMissing.WithMessage("payment_method.wallet.type is required").WithParam("payment_method.wallet.type")
		}
		req.PaymentMethod = &paymentv1.PaymentMethod{Method: &paymentv1.PaymentMethod_Wallet{Wallet: &paymentv1.Wallet{Type: pm.Wallet.Type, EncryptedPayload: pm.Wallet.EncryptedPayload}}}
	case "bank_transfer":
		if pm.BankTransfer == nil || pm.BankTransfer.Country == "" {
			return nil, apperr.ErrParameterMissing.WithMessage("payment_method.bank_transfer.country is required").WithParam("payment_method.bank_transfer.country")
		}
		req.PaymentMethod = &paymentv1.PaymentMethod{Method: &paymentv1.PaymentMethod_BankTransfer{BankTransfer: &paymentv1.BankTransfer{Country: pm.BankTransfer.Country, BankCode: pm.BankTransfer.BankCode, PayerName: pm.BankTransfer.PayerName}}}
	default:
		return nil, apperr.ErrParameterInvalid.WithMessage("payment_method.type must be card, wallet or bank_transfer").WithParam("payment_method.type")
	}
	if c := in.Customer; c != nil {
		req.Customer = &paymentv1.Customer{Id: c.ID, Email: c.Email, Name: c.Name, Phone: c.Phone, IpAddress: c.IPAddress, UserAgent: c.UserAgent, BillingCountry: c.BillingCountry, BillingPostalCode: c.BillingPostalCode}
	}
	return req, nil
}

type captureJSON struct {
	Amount *moneyJSON `json:"amount"`
	Final  *bool      `json:"final"`
}

type voidJSON struct {
	Reason string `json:"reason"`
}

type confirmJSON struct {
	ProviderParams map[string]string `json:"provider_params"`
}

type createRefundJSON struct {
	PaymentID string            `json:"payment_id"`
	Amount    *moneyJSON        `json:"amount"`
	Reason    string            `json:"reason"`
	Metadata  map[string]string `json:"metadata"`
}

type listJSON[T any] struct {
	Data       []T     `json:"data"`
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
}

type errorDetailJSON struct {
	Type    string  `json:"type"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

type nextActionJSON struct {
	Type             string         `json:"type"`
	Redirect         map[string]any `json:"redirect,omitempty"`
	ThreeDSChallenge map[string]any `json:"three_ds_challenge,omitempty"`
	Display          map[string]any `json:"display,omitempty"`
	ExpiresAt        *string        `json:"expires_at"`
}

type attemptJSON struct {
	ID                string  `json:"id"`
	Sequence          int32   `json:"sequence"`
	Provider          string  `json:"provider"`
	ProviderReference *string `json:"provider_reference"`
	Status            string  `json:"status"`
	ErrorCategory     *string `json:"error_category"`
	ErrorCode         *string `json:"error_code"`
	ErrorMessage      *string `json:"error_message"`
	RoutingReason     string  `json:"routing_reason"`
	CreatedAt         *string `json:"created_at"`
	CompletedAt       *string `json:"completed_at"`
}

type paymentMethodDetailsJSON struct {
	Type         string         `json:"type"`
	Card         map[string]any `json:"card,omitempty"`
	Wallet       map[string]any `json:"wallet,omitempty"`
	BankTransfer map[string]any `json:"bank_transfer,omitempty"`
}

type paymentJSON struct {
	ID                  string                    `json:"id"`
	Object              string                    `json:"object"`
	Amount              *moneyJSON                `json:"amount"`
	CapturedAmount      *moneyJSON                `json:"captured_amount"`
	RefundedAmount      *moneyJSON                `json:"refunded_amount"`
	Status              string                    `json:"status"`
	CaptureMethod       string                    `json:"capture_method"`
	PaymentMethod       *paymentMethodDetailsJSON `json:"payment_method"`
	Customer            *customerJSON             `json:"customer"`
	Metadata            map[string]string         `json:"metadata"`
	Description         *string                   `json:"description"`
	ReturnURL           *string                   `json:"return_url"`
	StatementDescriptor *string                   `json:"statement_descriptor"`
	Provider            *string                   `json:"provider"`
	ProviderReference   *string                   `json:"provider_reference"`
	NextAction          *nextActionJSON           `json:"next_action"`
	LastError           *errorDetailJSON          `json:"last_error"`
	Attempts            []attemptJSON             `json:"attempts"`
	LatestDisputeID     *string                   `json:"latest_dispute_id"`
	LiveMode            bool                      `json:"livemode"`
	CreatedAt           *string                   `json:"created_at"`
	UpdatedAt           *string                   `json:"updated_at"`
	AuthorizedAt        *string                   `json:"authorized_at"`
	CapturedAt          *string                   `json:"captured_at"`
	ExpiresAt           *string                   `json:"expires_at"`
}

type refundJSON struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	PaymentID         string            `json:"payment_id"`
	Amount            *moneyJSON        `json:"amount"`
	Status            string            `json:"status"`
	Reason            *string           `json:"reason"`
	Provider          *string           `json:"provider"`
	ProviderReference *string           `json:"provider_reference"`
	FailureCode       *string           `json:"failure_code"`
	FailureMessage    *string           `json:"failure_message"`
	Metadata          map[string]string `json:"metadata"`
	LiveMode          bool              `json:"livemode"`
	CreatedAt         *string           `json:"created_at"`
	UpdatedAt         *string           `json:"updated_at"`
	CompletedAt       *string           `json:"completed_at"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func tsString(t *timestamppb.Timestamp) *string {
	if t == nil {
		return nil
	}
	s := t.AsTime().UTC().Format(time.RFC3339)
	return &s
}

var paymentStatusNames = map[paymentv1.PaymentStatus]string{
	paymentv1.PaymentStatus_PAYMENT_STATUS_UNSPECIFIED:        "",
	paymentv1.PaymentStatus_PAYMENT_STATUS_CREATED:            "created",
	paymentv1.PaymentStatus_PAYMENT_STATUS_REQUIRES_ACTION:    "requires_action",
	paymentv1.PaymentStatus_PAYMENT_STATUS_AUTHORIZED:         "authorized",
	paymentv1.PaymentStatus_PAYMENT_STATUS_CAPTURED:           "captured",
	paymentv1.PaymentStatus_PAYMENT_STATUS_VOIDED:             "voided",
	paymentv1.PaymentStatus_PAYMENT_STATUS_PARTIALLY_REFUNDED: "partially_refunded",
	paymentv1.PaymentStatus_PAYMENT_STATUS_REFUNDED:           "refunded",
	paymentv1.PaymentStatus_PAYMENT_STATUS_DISPUTED:           "disputed",
	paymentv1.PaymentStatus_PAYMENT_STATUS_CHARGEBACK_WON:     "chargeback_won",
	paymentv1.PaymentStatus_PAYMENT_STATUS_CHARGEBACK_LOST:    "chargeback_lost",
	paymentv1.PaymentStatus_PAYMENT_STATUS_FAILED:             "failed",
	paymentv1.PaymentStatus_PAYMENT_STATUS_EXPIRED:            "expired",
}

func paymentStatusFromString(s string) (paymentv1.PaymentStatus, bool) {
	for k, v := range paymentStatusNames {
		if v == strings.TrimSpace(s) {
			return k, true
		}
	}
	return paymentv1.PaymentStatus_PAYMENT_STATUS_UNSPECIFIED, false
}

var attemptStatusNames = map[paymentv1.AttemptStatus]string{
	paymentv1.AttemptStatus_ATTEMPT_STATUS_UNSPECIFIED:     "",
	paymentv1.AttemptStatus_ATTEMPT_STATUS_PENDING:         "pending",
	paymentv1.AttemptStatus_ATTEMPT_STATUS_REQUIRES_ACTION: "requires_action",
	paymentv1.AttemptStatus_ATTEMPT_STATUS_APPROVED:        "approved",
	paymentv1.AttemptStatus_ATTEMPT_STATUS_DECLINED:        "declined",
	paymentv1.AttemptStatus_ATTEMPT_STATUS_UNAVAILABLE:     "unavailable",
	paymentv1.AttemptStatus_ATTEMPT_STATUS_UNKNOWN:         "unknown",
}

var categoryNames = map[providerv1.ProviderErrorCategory]string{
	providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_UNSPECIFIED:             "",
	providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_HARD:           "declined_hard",
	providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_DECLINED_SOFT:           "declined_soft",
	providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_PROVIDER_UNAVAILABLE:    "provider_unavailable",
	providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_INVALID_REQUEST:         "invalid_request",
	providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_FRAUD_SUSPECTED:         "fraud_suspected",
	providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_AUTHENTICATION_REQUIRED: "authentication_required",
	providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_UNKNOWN:                 "unknown",
}

func methodTypeName(t paymentv1.PaymentMethodType) string {
	switch t {
	case paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_CARD:
		return "card"
	case paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_WALLET:
		return "wallet"
	case paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_BANK_TRANSFER:
		return "bank_transfer"
	case paymentv1.PaymentMethodType_PAYMENT_METHOD_TYPE_UNSPECIFIED:
		return "card"
	default:
		return "card"
	}
}

func paymentToJSON(p *paymentv1.Payment, liveMode bool) *paymentJSON {
	if p == nil {
		return nil
	}
	out := &paymentJSON{
		ID: p.GetId(), Object: "payment", Amount: moneyFromProto(p.GetAmount()),
		CapturedAmount: moneyFromProto(p.GetCapturedAmount()), RefundedAmount: moneyFromProto(p.GetRefundedAmount()),
		Status: paymentStatusNames[p.GetStatus()], CaptureMethod: "automatic",
		Metadata: p.GetMetadata(), Description: nullable(p.GetDescription()), ReturnURL: nullable(p.GetReturnUrl()),
		StatementDescriptor: nullable(p.GetStatementDescriptor()), Provider: nullable(p.GetProvider()), ProviderReference: nullable(p.GetProviderReference()),
		LatestDisputeID: nullable(p.GetLatestDisputeId()), LiveMode: liveMode, Attempts: []attemptJSON{},
		CreatedAt: tsString(p.GetCreatedAt()), UpdatedAt: tsString(p.GetUpdatedAt()), AuthorizedAt: tsString(p.GetAuthorizedAt()),
		CapturedAt: tsString(p.GetCapturedAt()), ExpiresAt: tsString(p.GetExpiresAt()),
	}
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	if p.GetCaptureMethod() == paymentv1.CaptureMethod_CAPTURE_METHOD_MANUAL {
		out.CaptureMethod = "manual"
	}
	// 付款工具資訊。
	pmd := &paymentMethodDetailsJSON{Type: methodTypeName(p.GetPaymentMethodType())}
	if d := p.GetPaymentMethodDetails(); d != nil {
		fields := map[string]any{}
		for k, v := range map[string]string{"brand": d.GetBrand(), "last4": d.GetLast4(), "issuer": d.GetIssuer(), "issuer_country": d.GetIssuerCountry(), "funding": d.GetFunding(), "three_ds_result": d.GetThreeDsResult()} {
			if v != "" {
				fields[k] = v
			}
		}
		switch pmd.Type {
		case "wallet":
			pmd.Wallet = map[string]any{"type": d.GetBrand()}
		case "bank_transfer":
			pmd.BankTransfer = map[string]any{"bank_code": d.GetBrand(), "account_last4": d.GetLast4()}
		default:
			pmd.Card = fields
		}
	}
	out.PaymentMethod = pmd
	if c := p.GetCustomer(); c != nil {
		// 回應遮罩 ip_address / user_agent。
		out.Customer = &customerJSON{ID: c.GetId(), Email: c.GetEmail(), Name: c.GetName(), Phone: c.GetPhone(), BillingCountry: c.GetBillingCountry(), BillingPostalCode: c.GetBillingPostalCode()}
	}
	if na := p.GetNextAction(); na != nil {
		n := &nextActionJSON{ExpiresAt: tsString(na.GetExpiresAt())}
		switch a := na.GetAction().(type) {
		case *paymentv1.NextAction_Redirect:
			n.Type = "redirect"
			n.Redirect = map[string]any{"url": a.Redirect.GetUrl(), "method": a.Redirect.GetMethod()}
			if len(a.Redirect.GetFormFields()) > 0 {
				n.Redirect["form_fields"] = a.Redirect.GetFormFields()
			}
		case *paymentv1.NextAction_ThreeDsChallenge:
			n.Type = "three_ds_challenge"
			n.ThreeDSChallenge = map[string]any{"acs_url": a.ThreeDsChallenge.GetAcsUrl(), "creq": a.ThreeDsChallenge.GetCreq(), "transaction_id": a.ThreeDsChallenge.GetTransactionId(), "version": a.ThreeDsChallenge.GetVersion()}
		case *paymentv1.NextAction_Display:
			n.Type = "display"
			n.Display = map[string]any{"type": a.Display.GetType(), "details": a.Display.GetDetails()}
		}
		out.NextAction = n
	}
	if e := p.GetLastError(); e != nil {
		out.LastError = &errorDetailJSON{Type: e.GetType(), Code: e.GetCode(), Message: e.GetMessage(), Param: nullable(e.GetParam())}
	}
	for _, a := range p.GetAttempts() {
		aj := attemptJSON{
			ID: a.GetId(), Sequence: a.GetSequence(), Provider: a.GetProvider(), ProviderReference: nullable(a.GetProviderReference()),
			Status: attemptStatusNames[a.GetStatus()], ErrorCode: nullable(a.GetErrorCode()), ErrorMessage: nullable(a.GetErrorMessage()),
			RoutingReason: a.GetRoutingReason(), CreatedAt: tsString(a.GetCreatedAt()), CompletedAt: tsString(a.GetCompletedAt()),
		}
		if a.GetErrorCategory() != providerv1.ProviderErrorCategory_PROVIDER_ERROR_CATEGORY_UNSPECIFIED {
			aj.ErrorCategory = nullable(categoryNames[a.GetErrorCategory()])
		}
		out.Attempts = append(out.Attempts, aj)
	}
	return out
}

var refundStatusNames = map[paymentv1.RefundStatus]string{
	paymentv1.RefundStatus_REFUND_STATUS_UNSPECIFIED: "",
	paymentv1.RefundStatus_REFUND_STATUS_PENDING:     "pending",
	paymentv1.RefundStatus_REFUND_STATUS_SUCCEEDED:   "succeeded",
	paymentv1.RefundStatus_REFUND_STATUS_FAILED:      "failed",
}

func refundToJSON(r *paymentv1.Refund, liveMode bool) *refundJSON {
	if r == nil {
		return nil
	}
	out := &refundJSON{
		ID: r.GetId(), Object: "refund", PaymentID: r.GetPaymentId(), Amount: moneyFromProto(r.GetAmount()), Status: refundStatusNames[r.GetStatus()],
		Reason: nullable(r.GetReason()), Provider: nullable(r.GetProvider()), ProviderReference: nullable(r.GetProviderReference()),
		FailureCode: nullable(r.GetFailureCode()), FailureMessage: nullable(r.GetFailureMessage()), Metadata: r.GetMetadata(), LiveMode: liveMode,
		CreatedAt: tsString(r.GetCreatedAt()), UpdatedAt: tsString(r.GetUpdatedAt()), CompletedAt: tsString(r.GetCompletedAt()),
	}
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	return out
}

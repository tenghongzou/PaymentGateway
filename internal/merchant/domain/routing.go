package domain

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 路由規則的列舉值（docs/02 §9、proto RoutingRule）。
var (
	knownPaymentMethodTypes = map[string]struct{}{"card": {}, "wallet": {}, "bank_transfer": {}, "redirect": {}}
	knownCardBrands         = map[string]struct{}{"visa": {}, "mastercard": {}, "jcb": {}, "amex": {}, "unionpay": {}}
)

// RoutingRule 為一條路由規則；所有條件皆為 AND，空條件表示「任意」。
// JSON tag 即 routing_preferences.rules jsonb 的 schema。
type RoutingRule struct {
	Priority           int32    `json:"priority"`
	Currencies         []string `json:"currencies,omitempty"`
	PaymentMethodTypes []string `json:"payment_method_types,omitempty"`
	CardBrands         []string `json:"card_brands,omitempty"`
	Countries          []string `json:"countries,omitempty"`
	Provider           string   `json:"provider"`
	Enabled            bool     `json:"enabled"`
}

// RoutingPreferences 為商戶路由偏好。
//
// rules 存於 routing_preferences.rules；FallbackProviders / FailoverEnabled / MaxAttempts 因 DB 無專欄，
// 存於 merchants.settings（fallback_providers / allow_failover / max_attempts，見 docs/02 §2.1 MerchantSettings）。
type RoutingPreferences struct {
	MerchantID        uuid.UUID
	Rules             []RoutingRule
	FallbackProviders []string
	FailoverEnabled   bool
	MaxAttempts       int
	// UpdatedAt 為 nil 表示商戶從未設定（回傳系統預設）。
	UpdatedAt *time.Time
	Version   int
}

// DefaultRoutingPreferences 回傳系統預設（rules 空、failover true、max_attempts 3）。
func DefaultRoutingPreferences(merchantID uuid.UUID) *RoutingPreferences {
	return &RoutingPreferences{
		MerchantID:      merchantID,
		Rules:           []RoutingRule{},
		FailoverEnabled: true,
		MaxAttempts:     DefaultMaxAttempts,
	}
}

// ProviderSet 為已註冊的 adapter 識別碼集合；空集合表示不檢查。
type ProviderSet map[string]struct{}

// NewProviderSet 建立集合（小寫、去空白）。
func NewProviderSet(names []string) ProviderSet {
	s := ProviderSet{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" {
			s[n] = struct{}{}
		}
	}
	return s
}

// Known 檢查 provider 是否已註冊（空集合一律通過）。
func (s ProviderSet) Known(p string) bool {
	if len(s) == 0 {
		return true
	}
	_, ok := s[p]
	return ok
}

// Validate 檢查整份偏好：priority 唯一、provider 已知、幣別 / 國別格式、列舉值、max_attempts 範圍。
// 會就地正規化（幣別 / 國別轉大寫、provider / 列舉轉小寫、rules 依 priority 排序、MaxAttempts 0 → 預設）。
func (p *RoutingPreferences) Validate(providers ProviderSet) error {
	if p.MaxAttempts == 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	if p.MaxAttempts < MinMaxAttempts || p.MaxAttempts > MaxMaxAttempts {
		return invalid("max_attempts", "max_attempts must be between %d and %d", MinMaxAttempts, MaxMaxAttempts)
	}
	seen := map[int32]struct{}{}
	for i := range p.Rules {
		r := &p.Rules[i]
		param := "rules[" + strconv.Itoa(i) + "]"
		if _, dup := seen[r.Priority]; dup {
			return invalid(param+".priority", "duplicate priority %d", r.Priority)
		}
		seen[r.Priority] = struct{}{}
		r.Provider = strings.ToLower(strings.TrimSpace(r.Provider))
		if r.Provider == "" {
			return missing(param + ".provider")
		}
		if !providers.Known(r.Provider) {
			return invalid(param+".provider", "unknown provider %q", r.Provider)
		}
		for j, c := range r.Currencies {
			if err := ValidateCurrency(c, param+".currencies"); err != nil {
				return err
			}
			r.Currencies[j] = strings.ToUpper(c)
		}
		for j, c := range r.Countries {
			if err := ValidateCountry(c); err != nil {
				return invalid(param+".countries", "country must be an ISO 3166-1 alpha-2 code")
			}
			r.Countries[j] = strings.ToUpper(c)
		}
		for j, t := range r.PaymentMethodTypes {
			t = strings.ToLower(strings.TrimSpace(t))
			if _, ok := knownPaymentMethodTypes[t]; !ok {
				return invalid(param+".payment_method_types", "unknown payment method type %q", t)
			}
			r.PaymentMethodTypes[j] = t
		}
		for j, b := range r.CardBrands {
			b = strings.ToLower(strings.TrimSpace(b))
			if _, ok := knownCardBrands[b]; !ok {
				return invalid(param+".card_brands", "unknown card brand %q", b)
			}
			r.CardBrands[j] = b
		}
	}
	slices.SortStableFunc(p.Rules, func(a, b RoutingRule) int { return int(a.Priority - b.Priority) })
	for i, f := range p.FallbackProviders {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" || !providers.Known(f) {
			return invalid("fallback_providers", "unknown provider %q", f)
		}
		p.FallbackProviders[i] = f
	}
	if p.Rules == nil {
		p.Rules = []RoutingRule{}
	}
	return nil
}

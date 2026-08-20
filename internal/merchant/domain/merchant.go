package domain

import (
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// Status 為商戶狀態（與 migrations/merchant/0001 的 CHECK 一致）。
type Status string

// 商戶狀態全集。pending 只保留給 DB 相容（CreateMerchant 直接建立為 active）。
const (
	StatusPending   Status = "pending"
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusClosed    Status = "closed"
)

// ParseStatus 解析狀態字串。
func ParseStatus(s string) (Status, error) {
	switch Status(s) {
	case StatusPending, StatusActive, StatusSuspended, StatusClosed:
		return Status(s), nil
	default:
		return "", invalid("status", "unknown merchant status %q", s)
	}
}

// CanTransitionTo 實作 docs/02 §2.1 的狀態機：active ⇄ suspended → closed，closed 為終態；pending 可進入 active / closed。
func (s Status) CanTransitionTo(to Status) bool {
	switch s {
	case StatusPending:
		return to == StatusActive || to == StatusClosed
	case StatusActive:
		return to == StatusSuspended || to == StatusClosed
	case StatusSuspended:
		return to == StatusActive || to == StatusClosed
	case StatusClosed:
		return false
	default:
		return false
	}
}

// IsTerminal 回傳是否為終態。
func (s Status) IsTerminal() bool { return s == StatusClosed }

// CaptureMethod 為預設請款方式。
const (
	CaptureAutomatic = "automatic"
	CaptureManual    = "manual"
)

// 路由 / 重試相關預設值與上限（與 proto RoutingPreferences 註解一致）。
const (
	DefaultMaxAttempts = 3
	MinMaxAttempts     = 1
	MaxMaxAttempts     = 5
)

// Settings 為 merchants.settings jsonb 的 schema（docs/02 §2.1 MerchantSettings + proto Merchant 中 DB 無專欄的欄位）。
//
// 說明：migrations 的 merchants 表沒有 legal_name / contact_email / external_ref / statement_descriptor 專欄，
// Phase 0 一律放在 settings；external_ref 的唯一性由應用層檢查（TODO：補 migration 加專欄與 UNIQUE 索引）。
type Settings struct {
	LegalName           string `json:"legal_name,omitempty"`
	ContactEmail        string `json:"contact_email,omitempty"`
	StatementDescriptor string `json:"statement_descriptor,omitempty"`
	ExternalRef         string `json:"external_ref,omitempty"`
	// CaptureMethodDefault 為付款未指定 capture_method 時的預設（automatic / manual）。
	CaptureMethodDefault string `json:"capture_method_default,omitempty"`
	// MaxAttempts 為路由最大嘗試次數（1..5，0 = 預設 3）。
	MaxAttempts int `json:"max_attempts,omitempty"`
	// AllowFailover 為 nil 時視為 true。
	AllowFailover *bool `json:"allow_failover,omitempty"`
	// FallbackProviders 為沒有規則命中時依序嘗試的供應商。
	FallbackProviders []string `json:"fallback_providers,omitempty"`
	// TestModeEnabled 為 nil 時視為 true。
	TestModeEnabled *bool `json:"test_mode_enabled,omitempty"`
}

// EffectiveMaxAttempts 回傳套用預設後的 max_attempts。
func (s Settings) EffectiveMaxAttempts() int {
	if s.MaxAttempts == 0 {
		return DefaultMaxAttempts
	}
	return s.MaxAttempts
}

// EffectiveAllowFailover 回傳套用預設後的 allow_failover。
func (s Settings) EffectiveAllowFailover() bool {
	return s.AllowFailover == nil || *s.AllowFailover
}

// Merchant 為商戶聚合根（docs/02 §2.1）。
type Merchant struct {
	ID              uuid.UUID
	Name            string
	Status          Status
	Country         string
	DefaultCurrency string
	Settings        Settings
	Metadata        map[string]string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// Version 為樂觀鎖版本（UPDATE ... WHERE version = $n）。
	Version int
}

// PublicID 回傳對外 ID（mch_ + base32(uuid)），可由主鍵推導。
func (m *Merchant) PublicID() string { return ids.Format(ids.PrefixMerchant, m.ID) }

// ParseMerchantID 解析 mch_... 為 uuid。
func ParseMerchantID(publicID string) (uuid.UUID, error) {
	if publicID == "" {
		return uuid.Nil, missing("merchant_id")
	}
	u, err := ids.ParseWithPrefix(publicID, ids.PrefixMerchant)
	if err != nil {
		return uuid.Nil, invalid("merchant_id", "merchant_id must look like mch_<26 chars>")
	}
	return u, nil
}

// NewMerchantParams 為建立商戶的輸入。
type NewMerchantParams struct {
	Name                string
	LegalName           string
	Country             string
	DefaultCurrency     string
	ContactEmail        string
	StatementDescriptor string
	ExternalRef         string
	Metadata            map[string]string
}

// NewMerchant 建立商戶（狀態 active），並驗證所有欄位。
func NewMerchant(p NewMerchantParams, now time.Time) (*Merchant, error) {
	if err := ValidateName(p.Name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.LegalName) == "" {
		return nil, missing("legal_name")
	}
	if err := ValidateCountry(p.Country); err != nil {
		return nil, err
	}
	if err := ValidateCurrency(p.DefaultCurrency, "default_currency"); err != nil {
		return nil, err
	}
	if err := ValidateEmail(p.ContactEmail); err != nil {
		return nil, err
	}
	if err := ValidateStatementDescriptor(p.StatementDescriptor); err != nil {
		return nil, err
	}
	if err := ValidateMetadata(p.Metadata); err != nil {
		return nil, err
	}
	return &Merchant{
		ID:              ids.NewUUID(),
		Name:            strings.TrimSpace(p.Name),
		Status:          StatusActive,
		Country:         strings.ToUpper(p.Country),
		DefaultCurrency: strings.ToUpper(p.DefaultCurrency),
		Settings: Settings{
			LegalName:           strings.TrimSpace(p.LegalName),
			ContactEmail:        strings.TrimSpace(p.ContactEmail),
			StatementDescriptor: p.StatementDescriptor,
			ExternalRef:         strings.TrimSpace(p.ExternalRef),
		},
		Metadata:  cloneMetadata(p.Metadata),
		CreatedAt: now,
		UpdatedAt: now,
		Version:   0,
	}, nil
}

// Rename 更新顯示名稱。
func (m *Merchant) Rename(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	m.Name = strings.TrimSpace(name)
	return nil
}

// SetLegalName 更新法定名稱。
func (m *Merchant) SetLegalName(v string) error {
	if strings.TrimSpace(v) == "" {
		return missing("legal_name")
	}
	m.Settings.LegalName = strings.TrimSpace(v)
	return nil
}

// SetContactEmail 更新聯絡信箱。
func (m *Merchant) SetContactEmail(v string) error {
	if err := ValidateEmail(v); err != nil {
		return err
	}
	m.Settings.ContactEmail = strings.TrimSpace(v)
	return nil
}

// SetDefaultCurrency 更新預設幣別。
func (m *Merchant) SetDefaultCurrency(v string) error {
	if err := ValidateCurrency(v, "default_currency"); err != nil {
		return err
	}
	m.DefaultCurrency = strings.ToUpper(v)
	return nil
}

// SetStatementDescriptor 更新預設帳單描述。
func (m *Merchant) SetStatementDescriptor(v string) error {
	if err := ValidateStatementDescriptor(v); err != nil {
		return err
	}
	m.Settings.StatementDescriptor = v
	return nil
}

// SetMetadata 整份取代 metadata。
func (m *Merchant) SetMetadata(md map[string]string) error {
	if err := ValidateMetadata(md); err != nil {
		return err
	}
	m.Metadata = cloneMetadata(md)
	return nil
}

// Transition 依狀態機切換狀態；同狀態視為 no-op。
func (m *Merchant) Transition(to Status) error {
	if to == m.Status {
		return nil
	}
	if !m.Status.CanTransitionTo(to) {
		return ErrInvalidStateTransition.WithParam("status").
			WithMessage("cannot transition merchant from %s to %s", m.Status, to)
	}
	m.Status = to
	return nil
}

// AssertWritable 檢查商戶是否允許設定類寫入（docs/03 §2.5：suspended 允許、closed 拒絕）。
func (m *Merchant) AssertWritable() error {
	if m.Status == StatusClosed {
		return ErrMerchantClosed
	}
	return nil
}

// AssertCanCreatePayment 檢查商戶是否允許建立新付款（suspended / closed 皆拒絕）。
func (m *Merchant) AssertCanCreatePayment() error {
	switch m.Status {
	case StatusSuspended:
		return ErrMerchantSuspended
	case StatusClosed:
		return ErrMerchantClosed
	case StatusPending, StatusActive:
		return nil
	default:
		return nil
	}
}

// ---- 驗證規則 ----

const (
	maxNameLen                = 100
	maxStatementDescriptorLen = 22
	maxMetadataKeys           = 50
	maxMetadataKeyLen         = 40
	maxMetadataValueLen       = 500
)

var (
	countryRe  = regexp.MustCompile(`^[A-Z]{2}$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

// ValidateName 檢查顯示名稱（1–100 字）。
func ValidateName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return missing("name")
	}
	if len([]rune(n)) > maxNameLen {
		return invalid("name", "name must be at most %d characters", maxNameLen)
	}
	return nil
}

// ValidateCountry 檢查 ISO 3166-1 alpha-2（大小寫不敏感，儲存時轉大寫）。
func ValidateCountry(c string) error {
	if c == "" {
		return missing("country")
	}
	if !countryRe.MatchString(strings.ToUpper(c)) {
		return invalid("country", "country must be an ISO 3166-1 alpha-2 code")
	}
	return nil
}

// ValidateCurrency 檢查 ISO 4217 三碼格式（不檢查是否為已知幣別，交由 pkg/money）。
func ValidateCurrency(c, param string) error {
	if c == "" {
		return missing(param)
	}
	if !currencyRe.MatchString(strings.ToUpper(c)) {
		return invalid(param, "%s must be an ISO 4217 currency code", param)
	}
	return nil
}

// ValidateEmail 檢查 email 格式。
func ValidateEmail(e string) error {
	e = strings.TrimSpace(e)
	if e == "" {
		return missing("contact_email")
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e {
		return invalid("contact_email", "contact_email must be a valid email address")
	}
	return nil
}

// ValidateStatementDescriptor 檢查帳單描述（可空；最長 22、ASCII 可列印）。
func ValidateStatementDescriptor(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > maxStatementDescriptorLen {
		return invalid("default_statement_descriptor", "statement descriptor must be at most %d characters", maxStatementDescriptorLen)
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return invalid("default_statement_descriptor", "statement descriptor must contain only printable ASCII characters")
		}
	}
	return nil
}

// ValidateMetadata 檢查 metadata：最多 50 組、key ≤ 40、value ≤ 500、key 不可為空或以 _ 開頭（保留給內部欄位）。
func ValidateMetadata(md map[string]string) error {
	if len(md) > maxMetadataKeys {
		return ErrMetadataTooLarge.WithParam("metadata").WithMessage("metadata may contain at most %d keys", maxMetadataKeys)
	}
	for k, v := range md {
		if k == "" {
			return invalid("metadata", "metadata keys must not be empty")
		}
		if strings.HasPrefix(k, "_") {
			return invalid("metadata", "metadata keys must not start with '_' (reserved)")
		}
		if len(k) > maxMetadataKeyLen {
			return ErrMetadataTooLarge.WithParam("metadata").WithMessage("metadata key %q exceeds %d characters", k, maxMetadataKeyLen)
		}
		if len(v) > maxMetadataValueLen {
			return ErrMetadataTooLarge.WithParam("metadata").WithMessage("metadata value for %q exceeds %d characters", k, maxMetadataValueLen)
		}
	}
	return nil
}

func cloneMetadata(md map[string]string) map[string]string {
	out := make(map[string]string, len(md))
	for k, v := range md {
		out[k] = v
	}
	return out
}

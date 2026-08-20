package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// AccountType 為會計科目類型（accounts.type）。
type AccountType string

// 科目類型。
const (
	AccountTypeAsset     AccountType = "asset"
	AccountTypeLiability AccountType = "liability"
	AccountTypeRevenue   AccountType = "revenue"
	AccountTypeExpense   AccountType = "expense"
)

// NormalBalance 為科目的正常餘額方向（accounts.normal_balance）。
type NormalBalance string

// 正常餘額方向。
const (
	NormalDebit  NormalBalance = "debit"
	NormalCredit NormalBalance = "credit"
)

// NormalBalance 回傳科目類型對應的正常餘額方向（asset/expense → debit；liability/revenue → credit），
// 與 migrations 的 accounts_type_normal_balance CHECK 一致。
func (t AccountType) NormalBalance() NormalBalance {
	switch t {
	case AccountTypeAsset, AccountTypeExpense:
		return NormalDebit
	default:
		return NormalCredit
	}
}

// Valid 判斷科目類型是否合法。
func (t AccountType) Valid() bool {
	switch t {
	case AccountTypeAsset, AccountTypeLiability, AccountTypeRevenue, AccountTypeExpense:
		return true
	}
	return false
}

// AccountStatus 為帳戶狀態（accounts.status）。
type AccountStatus string

// 帳戶狀態。
const (
	AccountActive AccountStatus = "active"
	AccountFrozen AccountStatus = "frozen"
	AccountClosed AccountStatus = "closed"
)

// Level 為帳戶層級：系統帳戶（merchant_id IS NULL）或商戶帳戶。
type Level string

// 帳戶層級。
const (
	LevelSystem   Level = "system"
	LevelMerchant Level = "merchant"
)

// Kind 為科目代碼的「種類」部分（不含 provider / 銀行帳戶後綴），對應 docs/02 §7.1。
type Kind string

// Chart of Accounts 的 10 個科目（docs/02 §7.1、docs/04 §3.2.1）。
const (
	KindPSPReceivable        Kind = "psp_receivable"         // 1100 asset    系統  psp_receivable:<provider>
	KindBankCash             Kind = "bank_cash"              // 1200 asset    系統  bank_cash:<bank_account>
	KindSettlementSuspense   Kind = "settlement_suspense"    // 1900 asset    系統  settlement_suspense:<provider>
	KindMerchantPayable      Kind = "merchant_payable"       // 2100 liability 商戶
	KindRefundClearing       Kind = "refund_clearing"        // 2200 liability 商戶
	KindChargebackReserve    Kind = "chargeback_reserve"     // 2300 liability 商戶
	KindFeeRevenue           Kind = "fee_revenue"            // 4100 revenue  系統
	KindChargebackFeeRevenue Kind = "chargeback_fee_revenue" // 4200 revenue  系統
	KindPSPFeeExpense        Kind = "psp_fee_expense"        // 5100 expense  系統  psp_fee_expense:<provider>
	KindChargebackFeeExpense Kind = "chargeback_fee_expense" // 5200 expense  系統  chargeback_fee_expense:<provider>
)

// KindSpec 描述一個科目的建立規則。
type KindSpec struct {
	Kind Kind
	// LedgerCode 為 docs/04 §3.2.1 的會計編號（僅供報表 / 顯示）。
	LedgerCode string
	Type       AccountType
	Level      Level
	// Qualified 為 true 時 code 必須帶後綴（provider / 銀行帳戶），例：psp_receivable:stripe。
	Qualified bool
	// Name 為預設顯示名稱。
	Name string
}

// chart 為科目表；新增科目只需在此登記（accounts.code 在 SQL 沒有 CHECK，由 domain 驗證）。
var chart = map[Kind]KindSpec{
	KindPSPReceivable:        {Kind: KindPSPReceivable, LedgerCode: "1100", Type: AccountTypeAsset, Level: LevelSystem, Qualified: true, Name: "PSP receivable"},
	KindBankCash:             {Kind: KindBankCash, LedgerCode: "1200", Type: AccountTypeAsset, Level: LevelSystem, Qualified: true, Name: "Bank cash"},
	KindSettlementSuspense:   {Kind: KindSettlementSuspense, LedgerCode: "1900", Type: AccountTypeAsset, Level: LevelSystem, Qualified: true, Name: "Settlement suspense"},
	KindMerchantPayable:      {Kind: KindMerchantPayable, LedgerCode: "2100", Type: AccountTypeLiability, Level: LevelMerchant, Name: "Merchant payable"},
	KindRefundClearing:       {Kind: KindRefundClearing, LedgerCode: "2200", Type: AccountTypeLiability, Level: LevelMerchant, Name: "Refund clearing"},
	KindChargebackReserve:    {Kind: KindChargebackReserve, LedgerCode: "2300", Type: AccountTypeLiability, Level: LevelMerchant, Name: "Chargeback reserve"},
	KindFeeRevenue:           {Kind: KindFeeRevenue, LedgerCode: "4100", Type: AccountTypeRevenue, Level: LevelSystem, Name: "Fee revenue"},
	KindChargebackFeeRevenue: {Kind: KindChargebackFeeRevenue, LedgerCode: "4200", Type: AccountTypeRevenue, Level: LevelSystem, Name: "Chargeback fee revenue"},
	KindPSPFeeExpense:        {Kind: KindPSPFeeExpense, LedgerCode: "5100", Type: AccountTypeExpense, Level: LevelSystem, Qualified: true, Name: "PSP fee expense"},
	KindChargebackFeeExpense: {Kind: KindChargebackFeeExpense, LedgerCode: "5200", Type: AccountTypeExpense, Level: LevelSystem, Qualified: true, Name: "Chargeback fee expense"},
}

// chartOrder 固定 ChartOfAccounts() 的輸出順序（依會計編號）。
var chartOrder = []Kind{
	KindPSPReceivable, KindBankCash, KindSettlementSuspense,
	KindMerchantPayable, KindRefundClearing, KindChargebackReserve,
	KindFeeRevenue, KindChargebackFeeRevenue,
	KindPSPFeeExpense, KindChargebackFeeExpense,
}

// ChartOfAccounts 回傳全部科目規格（依會計編號排序）。
func ChartOfAccounts() []KindSpec {
	out := make([]KindSpec, 0, len(chartOrder))
	for _, k := range chartOrder {
		out = append(out, chart[k])
	}
	return out
}

// SpecOf 取得科目規格；未知 kind 回 ok=false。
func SpecOf(kind Kind) (KindSpec, bool) {
	s, ok := chart[kind]
	return s, ok
}

// qualifierRe 限制後綴字元（provider 名稱 / 銀行帳戶代碼）。
var qualifierRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// testCodePrefix 為 test-mode 帳戶的 code 前綴。
//
// migrations/ledger 的 accounts 沒有 livemode 欄位、唯一鍵為 (merchant_id, code, currency)，
// 為了讓 test-mode 事件記到獨立的試算帳本而不與正式餘額混在一起，test 帳戶以 code 前綴 "test:" 區隔。
// TODO(ledger/migration): 建議新增 accounts.livemode 欄位並納入唯一鍵，屆時移除此前綴。
const testCodePrefix = "test:"

// ParseCode 解析完整科目代碼（例：psp_receivable:stripe、merchant_payable），回傳種類與後綴。
func ParseCode(code string) (Kind, string, error) {
	kindStr, qualifier, hasQualifier := strings.Cut(code, ":")
	spec, ok := chart[Kind(kindStr)]
	if !ok {
		return "", "", ErrInvalidAccountCode.WithMessage("unknown account code %q", code)
	}
	if spec.Qualified {
		if !hasQualifier || !qualifierRe.MatchString(qualifier) {
			return "", "", ErrInvalidAccountCode.WithMessage("account code %q requires a qualifier (e.g. %s:<provider>)", code, kindStr)
		}
		return spec.Kind, qualifier, nil
	}
	if hasQualifier {
		return "", "", ErrInvalidAccountCode.WithMessage("account code %q must not have a qualifier", code)
	}
	return spec.Kind, "", nil
}

// CodeFor 組出完整科目代碼。
func CodeFor(kind Kind, qualifier string) (string, error) {
	spec, ok := chart[kind]
	if !ok {
		return "", ErrInvalidAccountCode.WithMessage("unknown account kind %q", kind)
	}
	if spec.Qualified {
		if !qualifierRe.MatchString(qualifier) {
			return "", ErrInvalidAccountCode.WithMessage("account kind %q requires a qualifier matching %s", kind, qualifierRe)
		}
		return string(kind) + ":" + qualifier, nil
	}
	if qualifier != "" {
		return "", ErrInvalidAccountCode.WithMessage("account kind %q must not have a qualifier", kind)
	}
	return string(kind), nil
}

// StorageCode 把 (code, livemode) 轉為 accounts.code 的實際儲存值。
func StorageCode(code string, livemode bool) string {
	if livemode {
		return code
	}
	return testCodePrefix + code
}

// ParseStorageCode 把 accounts.code 還原為 (code, livemode)。
func ParseStorageCode(stored string) (code string, livemode bool) {
	if rest, ok := strings.CutPrefix(stored, testCodePrefix); ok {
		return rest, false
	}
	return stored, true
}

// AccountKey 為帳戶的自然鍵：merchant × code × currency（× livemode），對應 accounts 的唯一鍵。
// MerchantID 為 uuid.Nil 代表系統帳戶（DB 中 merchant_id IS NULL）。
type AccountKey struct {
	MerchantID uuid.UUID
	Code       string
	Currency   string
	Livemode   bool
}

// IsSystem 回傳是否為系統帳戶。
func (k AccountKey) IsSystem() bool { return k.MerchantID == uuid.Nil }

// Kind 回傳科目種類（code 不合法時為空）。
func (k AccountKey) Kind() Kind {
	kind, _, _ := ParseCode(k.Code)
	return kind
}

// Qualifier 回傳 code 的後綴（provider / bank account）。
func (k AccountKey) Qualifier() string {
	_, q, _ := ParseCode(k.Code)
	return q
}

// Validate 檢查 code 在科目表中、系統 / 商戶層級與 merchant_id 相符、幣別受支援。
func (k AccountKey) Validate() error {
	kind, _, err := ParseCode(k.Code)
	if err != nil {
		return err
	}
	spec := chart[kind]
	switch spec.Level {
	case LevelSystem:
		if !k.IsSystem() {
			return ErrInvalidAccountCode.WithMessage("account code %q is a system account and must not have a merchant_id", k.Code)
		}
	case LevelMerchant:
		if k.IsSystem() {
			return ErrInvalidAccountCode.WithMessage("account code %q is a merchant account and requires a merchant_id", k.Code)
		}
	}
	if !money.IsSupportedCurrency(k.Currency) {
		return ErrInvalidCurrency.WithMessage("unsupported currency %q", k.Currency)
	}
	return nil
}

// String 供 log / 錯誤訊息使用。
func (k AccountKey) String() string {
	owner := "platform"
	if !k.IsSystem() {
		owner = k.MerchantID.String()
	}
	mode := "live"
	if !k.Livemode {
		mode = "test"
	}
	return fmt.Sprintf("%s[%s,%s,%s]", k.Code, owner, k.Currency, mode)
}

// --- 科目建構輔助（讓分錄範本與測試可讀）---

// PSPReceivable 系統資產：psp_receivable:<provider>。
func PSPReceivable(provider, currency string, livemode bool) AccountKey {
	return AccountKey{Code: string(KindPSPReceivable) + ":" + provider, Currency: currency, Livemode: livemode}
}

// BankCash 系統資產：bank_cash:<bank_account>。
func BankCash(bankAccount, currency string, livemode bool) AccountKey {
	return AccountKey{Code: string(KindBankCash) + ":" + bankAccount, Currency: currency, Livemode: livemode}
}

// SettlementSuspense 系統資產：settlement_suspense:<provider>。
func SettlementSuspense(provider, currency string, livemode bool) AccountKey {
	return AccountKey{Code: string(KindSettlementSuspense) + ":" + provider, Currency: currency, Livemode: livemode}
}

// MerchantPayable 商戶負債：merchant_payable。
func MerchantPayable(merchantID uuid.UUID, currency string, livemode bool) AccountKey {
	return AccountKey{MerchantID: merchantID, Code: string(KindMerchantPayable), Currency: currency, Livemode: livemode}
}

// RefundClearing 商戶負債：refund_clearing。
func RefundClearing(merchantID uuid.UUID, currency string, livemode bool) AccountKey {
	return AccountKey{MerchantID: merchantID, Code: string(KindRefundClearing), Currency: currency, Livemode: livemode}
}

// ChargebackReserve 商戶負債：chargeback_reserve。
func ChargebackReserve(merchantID uuid.UUID, currency string, livemode bool) AccountKey {
	return AccountKey{MerchantID: merchantID, Code: string(KindChargebackReserve), Currency: currency, Livemode: livemode}
}

// FeeRevenue 系統收入：fee_revenue。
func FeeRevenue(currency string, livemode bool) AccountKey {
	return AccountKey{Code: string(KindFeeRevenue), Currency: currency, Livemode: livemode}
}

// ChargebackFeeRevenue 系統收入：chargeback_fee_revenue。
func ChargebackFeeRevenue(currency string, livemode bool) AccountKey {
	return AccountKey{Code: string(KindChargebackFeeRevenue), Currency: currency, Livemode: livemode}
}

// PSPFeeExpense 系統費用：psp_fee_expense:<provider>。
func PSPFeeExpense(provider, currency string, livemode bool) AccountKey {
	return AccountKey{Code: string(KindPSPFeeExpense) + ":" + provider, Currency: currency, Livemode: livemode}
}

// ChargebackFeeExpense 系統費用：chargeback_fee_expense:<provider>。
func ChargebackFeeExpense(provider, currency string, livemode bool) AccountKey {
	return AccountKey{Code: string(KindChargebackFeeExpense) + ":" + provider, Currency: currency, Livemode: livemode}
}

// Account 為帳戶聚合根（accounts 表一列）。
type Account struct {
	ID            uuid.UUID
	Key           AccountKey
	Name          string
	Type          AccountType
	NormalBalance NormalBalance
	Status        AccountStatus
	Metadata      map[string]string
	Version       int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NewAccount 依科目表規則建立帳戶（尚未持久化；ID 由呼叫端或 DB 指定）。
func NewAccount(key AccountKey) (*Account, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	spec := chart[key.Kind()]
	return &Account{
		Key:           key,
		Name:          DefaultAccountName(key),
		Type:          spec.Type,
		NormalBalance: spec.Type.NormalBalance(),
		Status:        AccountActive,
		Metadata:      map[string]string{},
	}, nil
}

// DefaultAccountName 產生顯示名稱，例：「merchant_payable/TWD/<merchant uuid>」、「psp_receivable:stripe/TWD/platform」。
func DefaultAccountName(key AccountKey) string {
	owner := "platform"
	if !key.IsSystem() {
		owner = key.MerchantID.String()
	}
	name := key.Code + "/" + key.Currency + "/" + owner
	if !key.Livemode {
		name += " (test)"
	}
	return name
}

// CanPost 回傳帳戶是否可被過帳（只有 active；與 entries_before_insert trigger 一致）。
func (a *Account) CanPost() bool { return a.Status == AccountActive }

// Kind 回傳科目種類。
func (a *Account) Kind() Kind { return a.Key.Kind() }

// SignedDelta 回傳一筆分錄對此帳戶餘額的帶號影響（以 normal_balance 方向為正），
// 與 entries_apply_balance trigger 的算法相同。
func (a *Account) SignedDelta(dir Direction, amountMinor int64) int64 {
	return signedDelta(a.NormalBalance, dir, amountMinor)
}

func signedDelta(normal NormalBalance, dir Direction, amountMinor int64) int64 {
	if string(dir) == string(normal) {
		return amountMinor
	}
	return -amountMinor
}

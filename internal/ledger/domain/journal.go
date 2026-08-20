package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// Direction 為分錄方向（entries.direction）。
type Direction string

// 分錄方向。
const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Valid 判斷方向是否合法。
func (d Direction) Valid() bool { return d == Debit || d == Credit }

// Opposite 回傳相反方向（沖銷用）。
func (d Direction) Opposite() Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

// ReferenceType 為 journal 的業務參照類型（journals.reference_type CHECK）。
type ReferenceType string

// 業務參照類型。
const (
	RefPayment    ReferenceType = "payment"
	RefRefund     ReferenceType = "refund"
	RefDispute    ReferenceType = "dispute"
	RefFee        ReferenceType = "fee"
	RefSettlement ReferenceType = "settlement"
	RefAdjustment ReferenceType = "adjustment"
	RefReversal   ReferenceType = "reversal"
)

// Valid 判斷 reference_type 是否在 DB CHECK 清單內。
func (r ReferenceType) Valid() bool {
	switch r {
	case RefPayment, RefRefund, RefDispute, RefFee, RefSettlement, RefAdjustment, RefReversal:
		return true
	}
	return false
}

// SourceType 為 journal 的來源（對應 proto JournalSourceType；DB 存於 metadata.source_type）。
type SourceType string

// 來源類型。
const (
	SourcePaymentEvent             SourceType = "payment_event"
	SourceReconciliationAdjustment SourceType = "reconciliation_adjustment"
	SourceManualAdjustment         SourceType = "manual_adjustment"
	SourceReversal                 SourceType = "reversal"
	SourcePayout                   SourceType = "payout"
)

// journals.metadata 內由系統保留的鍵。
const (
	MetaTemplate   = "template"    // 分錄範本 ID（J-CAP …）
	MetaSourceType = "source_type" // SourceType
	MetaSourceID   = "source_id"   // 來源 ID（event_id / 被沖銷 journal / 工單）
	MetaPaymentID  = "payment_id"  // 退款 / 爭議 journal 與 payment 的關聯（docs/02 §7.6 第 9 點）
	MetaLivemode   = "livemode"    // "true" / "false"
	MetaProvider   = "provider"
)

// Entry 為 journal 內的一筆分錄。
//
// 分錄範本產生時只知道帳戶的自然鍵（Account），AccountID 由 app 層在過帳交易內 lazy create 後填入。
type Entry struct {
	ID          uuid.UUID
	AccountID   uuid.UUID
	Account     AccountKey
	Direction   Direction
	Amount      money.Money
	Description string
	CreatedAt   time.Time
}

// Journal 為日記帳聚合根（journals 表一列 + entries）。post 後不可修改。
type Journal struct {
	ID       uuid.UUID
	PublicID string
	// EventID 為冪等鍵（journals.event_id UNIQUE）：payment 事件的 event_id、或 gRPC PostJournal 的 idempotency_key。
	EventID uuid.UUID
	// MerchantID 為所屬商戶；跨商戶的系統 journal（例如 J-STL）為 uuid.Nil
	// （DB 欄位 NOT NULL，adapter 以 uuid.Nil 寫入；見 docs/02 §7.3 J-STL 備註）。
	MerchantID    uuid.UUID
	Livemode      bool
	SourceType    SourceType
	SourceID      string
	ReferenceType ReferenceType
	ReferenceID   string
	Description   string
	// Template 為產生此 journal 的範本 ID（J-CAP、J-REF-OK …；人工過帳為空）。
	Template string
	// ReversalOf 指向被沖銷的 journal；ReversedBy 為沖銷本 journal 的 journal（讀取時填入）。
	ReversalOf *uuid.UUID
	ReversedBy *uuid.UUID
	// EffectiveAt 為業務發生時間（事件 occurred_at）；PostedAt 為入帳時間。
	EffectiveAt time.Time
	PostedAt    time.Time
	Metadata    map[string]string
	Entries     []Entry
}

// Currency 回傳 journal 幣別（取自第一筆分錄；無分錄時為空）。
func (j *Journal) Currency() string {
	if len(j.Entries) == 0 {
		return ""
	}
	return j.Entries[0].Amount.Currency
}

// TotalDebit 回傳借方合計。
func (j *Journal) TotalDebit() int64 { return j.total(Debit) }

// TotalCredit 回傳貸方合計。
func (j *Journal) TotalCredit() int64 { return j.total(Credit) }

func (j *Journal) total(dir Direction) int64 {
	var sum int64
	for _, e := range j.Entries {
		if e.Direction == dir {
			sum += e.Amount.AmountMinor
		}
	}
	return sum
}

// Validate 檢查帳本不變條件（docs/02 §7.6 1–3）：
//   - 至少 2 筆分錄、金額 > 0、方向合法
//   - 所有分錄幣別一致且等於帳戶幣別、帳戶 code 合法
//   - 商戶帳戶必須屬於 journal 的商戶；帳戶 livemode 與 journal 一致
//   - Σ借 = Σ貸
//   - event_id 必填、reference_type 合法
func (j *Journal) Validate() error {
	if j.EventID == uuid.Nil {
		return ErrEventIDMissing
	}
	if !j.ReferenceType.Valid() {
		return ErrReferenceTypeInvalid.WithMessage("reference_type %q is invalid", j.ReferenceType)
	}
	if len(j.Entries) < 2 {
		return ErrJournalTooFewEntries
	}
	currency := j.Entries[0].Amount.Currency
	var debits, credits int64
	for i, e := range j.Entries {
		if !e.Direction.Valid() {
			return ErrEntryDirectionInvalid.WithMessage("entries[%d]: direction %q is invalid", i, e.Direction)
		}
		if err := e.Amount.Validate(); err != nil {
			return ErrInvalidCurrency.WithMessage("entries[%d]: %v", i, err)
		}
		if !e.Amount.IsPositive() {
			return ErrEntryAmountInvalid.WithMessage("entries[%d]: amount must be > 0", i)
		}
		if e.Amount.Currency != currency {
			return ErrJournalCurrencyMismatch.WithMessage("entries[%d]: currency %s differs from %s", i, e.Amount.Currency, currency)
		}
		if err := e.Account.Validate(); err != nil {
			return err
		}
		if e.Account.Currency != currency {
			return ErrJournalCurrencyMismatch.WithMessage("entries[%d]: account %s currency differs from entry currency %s", i, e.Account, currency)
		}
		if e.Account.Livemode != j.Livemode {
			return ErrAccountLivemodeMismatch.WithMessage("entries[%d]: account %s livemode differs from journal", i, e.Account)
		}
		if !e.Account.IsSystem() && j.MerchantID != uuid.Nil && e.Account.MerchantID != j.MerchantID {
			return ErrAccountMerchantMismatch.WithMessage("entries[%d]: account %s belongs to another merchant", i, e.Account)
		}
		// 逐筆累加時檢查溢位（金額皆 > 0，只會往上溢位）。
		if e.Direction == Debit {
			if debits > maxInt64-e.Amount.AmountMinor {
				return ErrEntryAmountInvalid.WithMessage("debit total overflows int64")
			}
			debits += e.Amount.AmountMinor
		} else {
			if credits > maxInt64-e.Amount.AmountMinor {
				return ErrEntryAmountInvalid.WithMessage("credit total overflows int64")
			}
			credits += e.Amount.AmountMinor
		}
	}
	if debits != credits {
		return ErrJournalUnbalanced.WithMessage("journal is unbalanced: debits=%d credits=%d %s", debits, credits, currency)
	}
	return nil
}

const maxInt64 = int64(^uint64(0) >> 1)

// AccountKeys 回傳 journal 觸及的帳戶自然鍵（去重、保持首次出現順序），供 lazy create。
func (j *Journal) AccountKeys() []AccountKey {
	seen := map[AccountKey]struct{}{}
	out := make([]AccountKey, 0, len(j.Entries))
	for _, e := range j.Entries {
		if _, ok := seen[e.Account]; ok {
			continue
		}
		seen[e.Account] = struct{}{}
		out = append(out, e.Account)
	}
	return out
}

// Reverse 建立 orig 的沖銷 journal（J-REV，docs/02 §7.3）：每筆分錄方向對調、金額相同，
// reference_type = reversal、reversal_of 指向原 journal。eventID 為新 journal 的冪等鍵。
// 原 journal 已被沖銷（ReversedBy 非 nil）時回 ErrJournalAlreadyReversed（應用層仍須在交易內以 DB 再查一次）。
func Reverse(orig *Journal, eventID uuid.UUID, description string, now time.Time) (*Journal, error) {
	if orig == nil {
		return nil, ErrJournalNotFound
	}
	if orig.ReversedBy != nil {
		return nil, ErrJournalAlreadyReversed
	}
	if orig.ID == uuid.Nil {
		return nil, ErrJournalNotFound.WithMessage("cannot reverse an unsaved journal")
	}
	if description == "" {
		description = "Reversal of " + orig.PublicID
	}
	origID := orig.ID
	rev := &Journal{
		EventID:       eventID,
		MerchantID:    orig.MerchantID,
		Livemode:      orig.Livemode,
		SourceType:    SourceReversal,
		SourceID:      orig.PublicID,
		ReferenceType: RefReversal,
		ReferenceID:   orig.PublicID,
		Description:   description,
		Template:      TemplateJREV,
		ReversalOf:    &origID,
		EffectiveAt:   now,
		PostedAt:      now,
		Metadata:      map[string]string{},
		Entries:       make([]Entry, 0, len(orig.Entries)),
	}
	for k, v := range orig.Metadata {
		// 保留業務關聯（payment_id 等），但範本 / 來源由沖銷本身決定。
		if k == MetaTemplate || k == MetaSourceType || k == MetaSourceID {
			continue
		}
		rev.Metadata[k] = v
	}
	for _, e := range orig.Entries {
		rev.Entries = append(rev.Entries, Entry{
			AccountID:   e.AccountID,
			Account:     e.Account,
			Direction:   e.Direction.Opposite(),
			Amount:      e.Amount,
			Description: "reversal: " + e.Description,
		})
	}
	if err := rev.Validate(); err != nil {
		return nil, err
	}
	return rev, nil
}

// ValidateReversal 檢查 rev 是否為 orig 的合法沖銷：分錄多重集合必須為 orig 的鏡像（同帳戶、同金額、方向相反）。
// 供 gRPC PostJournal(reverses_journal_id) 等由呼叫端自行提供分錄的情境使用。
func ValidateReversal(orig, rev *Journal) error {
	if orig == nil {
		return ErrJournalNotFound
	}
	if orig.ReversedBy != nil {
		return ErrJournalAlreadyReversed
	}
	if len(orig.Entries) != len(rev.Entries) {
		return ErrReversalMismatch
	}
	type line struct {
		acct AccountKey
		dir  Direction
		amt  int64
	}
	expect := map[line]int{}
	for _, e := range orig.Entries {
		expect[line{e.Account, e.Direction.Opposite(), e.Amount.AmountMinor}]++
	}
	for _, e := range rev.Entries {
		l := line{e.Account, e.Direction, e.Amount.AmountMinor}
		if expect[l] == 0 {
			return ErrReversalMismatch
		}
		expect[l]--
	}
	if rev.ReversalOf == nil || *rev.ReversalOf != orig.ID {
		return ErrReversalMismatch.WithMessage("reversal_of must point to %s", orig.PublicID)
	}
	return nil
}

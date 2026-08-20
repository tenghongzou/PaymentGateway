package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Balances 為記憶體內的餘額讀模型：AccountKey → 帶號餘額（以 normal_balance 方向為正），
// 算法與 entries_apply_balance trigger 相同。供 property-based 測試、fake repo 與試算平衡檢查使用。
type Balances map[AccountKey]int64

// Apply 把 journal 的每筆分錄套用到餘額上（先 Validate，不平衡的 journal 不會被套用）。
func (b Balances) Apply(j *Journal) error {
	if err := j.Validate(); err != nil {
		return err
	}
	for _, e := range j.Entries {
		spec := chart[e.Account.Kind()]
		b[e.Account] += signedDelta(spec.Type.NormalBalance(), e.Direction, e.Amount.AmountMinor)
	}
	return nil
}

// Of 回傳帳戶餘額（不存在為 0）。
func (b Balances) Of(key AccountKey) int64 { return b[key] }

// TrialBalance 為某幣別 / livemode 的試算平衡彙總。
type TrialBalance struct {
	Currency    string
	Livemode    bool
	Assets      int64
	Liabilities int64
	Revenue     int64
	Expense     int64
}

// Equity 回傳權益（收入 − 費用）。
func (t TrialBalance) Equity() int64 { return t.Revenue - t.Expense }

// Balanced 檢查會計恆等式 Σassets = Σliabilities + Σrevenue − Σexpense。
func (t TrialBalance) Balanced() bool { return t.Assets == t.Liabilities+t.Equity() }

// TrialBalances 依 (currency, livemode) 分組彙總。
func (b Balances) TrialBalances() map[string]TrialBalance {
	out := map[string]TrialBalance{}
	for key, bal := range b {
		groupKey := fmt.Sprintf("%s/%t", key.Currency, key.Livemode)
		t := out[groupKey]
		t.Currency, t.Livemode = key.Currency, key.Livemode
		switch chart[key.Kind()].Type {
		case AccountTypeAsset:
			t.Assets += bal
		case AccountTypeLiability:
			t.Liabilities += bal
		case AccountTypeRevenue:
			t.Revenue += bal
		case AccountTypeExpense:
			t.Expense += bal
		}
		out[groupKey] = t
	}
	return out
}

// CheckIdentity 回傳第一個不滿足恆等式的分組錯誤（全部平衡時為 nil）。
func (b Balances) CheckIdentity() error {
	for _, t := range b.TrialBalances() {
		if !t.Balanced() {
			return fmt.Errorf("ledger: accounting identity violated for %s (livemode=%t): assets=%d liabilities=%d revenue=%d expense=%d",
				t.Currency, t.Livemode, t.Assets, t.Liabilities, t.Revenue, t.Expense)
		}
	}
	return nil
}

// Clone 回傳複本（測試比對用）。
func (b Balances) Clone() Balances {
	c := make(Balances, len(b))
	for k, v := range b {
		c[k] = v
	}
	return c
}

// Equal 判斷兩份餘額是否相同（忽略值為 0 的項目）。
func (b Balances) Equal(o Balances) bool {
	for k, v := range b {
		if v != o[k] {
			return false
		}
	}
	for k, v := range o {
		if v != b[k] {
			return false
		}
	}
	return true
}

// Balance 為單一帳戶的即時餘額讀模型（balances 表一列 + 帳戶資訊；docs/02 §2.3）。
type Balance struct {
	AccountID     uuid.UUID
	Account       AccountKey
	Type          AccountType
	NormalBalance NormalBalance
	// Balance 以 normal_balance 方向為正，可為負。
	Balance int64
	// TotalDebit / TotalCredit 為借貸合計（由 entries 彙總；未彙總時為 0）。
	TotalDebit  int64
	TotalCredit int64
	EntryCount  int64
	// AsOfEntryID / AsOfJournalID 為最後一筆納入計算的分錄 / journal。
	AsOfEntryID   *uuid.UUID
	AsOfJournalID *uuid.UUID
	Version       int64
	UpdatedAt     time.Time
}

// MerchantBalance 為商戶視角的餘額拆解（proto MerchantBalance）。
//
// 依 docs/02 §7.3 的範本，退款預扣（J-REF-PEND）與爭議凍結（J-CB-OPEN）都已經從 merchant_payable
// 移出到 refund_clearing / chargeback_reserve，因此 Available = Payable；Pending / Reserved 僅供揭露。
type MerchantBalance struct {
	MerchantID uuid.UUID
	Currency   string
	Livemode   bool
	Payable    int64
	Pending    int64
	Reserved   int64
	Available  int64
	AsOf       time.Time
}

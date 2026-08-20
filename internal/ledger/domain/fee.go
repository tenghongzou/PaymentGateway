package domain

import (
	"math/big"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// FeePolicy 為手續費模型（docs/02 §7.4）：fee = clamp(fixed + round_half_up(amount × bps / 10000), min, max)，且 fee ≤ amount。
//
// ledger-service 正常情況下只依事件中的 fee_amount 記帳；本型別用於抽樣複算與測試。
type FeePolicy struct {
	// FixedMinor 為固定費（最小單位）。
	FixedMinor int64
	// PercentageBps 為百分比（basis point，1 bps = 0.01%）。
	PercentageBps int64
	// MinFeeMinor / MaxFeeMinor 為 clamp 範圍；<= 0 表示不設定。
	MinFeeMinor int64
	MaxFeeMinor int64
}

// FeeCalculation 為一次手續費計算的結果（值物件）。
type FeeCalculation struct {
	Base           money.Money
	Fixed          money.Money
	PercentagePart money.Money
	Total          money.Money
	// Clamped 表示結果曾被 min/max 或「不可超過金額」規則修正。
	Clamped bool
}

// Calculate 依政策計算手續費。
func (p FeePolicy) Calculate(amount money.Money) (FeeCalculation, error) {
	if err := amount.Validate(); err != nil {
		return FeeCalculation{}, ErrInvalidCurrency.Wrap(err)
	}
	if p.FixedMinor < 0 || p.PercentageBps < 0 {
		return FeeCalculation{}, ErrEventInvalid.WithMessage("fee policy must not be negative")
	}
	pct, err := amount.MulBps(p.PercentageBps)
	if err != nil {
		return FeeCalculation{}, ErrEventInvalid.Wrap(err)
	}
	fixed := money.Money{AmountMinor: p.FixedMinor, Currency: amount.Currency}
	total, err := fixed.Add(pct)
	if err != nil {
		return FeeCalculation{}, ErrEventInvalid.Wrap(err)
	}
	clamped := false
	if p.MinFeeMinor > 0 && total.AmountMinor < p.MinFeeMinor {
		total.AmountMinor = p.MinFeeMinor
		clamped = true
	}
	if p.MaxFeeMinor > 0 && total.AmountMinor > p.MaxFeeMinor {
		total.AmountMinor = p.MaxFeeMinor
		clamped = true
	}
	// 手續費不可超過金額（docs/02 §7.4 第 5 步）。
	if total.AmountMinor > amount.AmountMinor {
		total.AmountMinor = amount.AmountMinor
		clamped = true
	}
	return FeeCalculation{Base: amount, Fixed: fixed, PercentagePart: pct, Total: total, Clamped: clamped}, nil
}

// ProRataFee 計算 round_half_up(fee × part / total)，用於 J-REF-FEE-RET（退回手續費的百分比部分）。
// 以 big.Int 計算避免 int64 溢位；total 為 0 時回 0。
func ProRataFee(fee, part, total money.Money) (money.Money, error) {
	if fee.Currency != part.Currency || fee.Currency != total.Currency {
		return money.Money{}, ErrJournalCurrencyMismatch.Wrap(money.ErrCurrencyMismatch)
	}
	if total.AmountMinor <= 0 || part.AmountMinor <= 0 || fee.AmountMinor <= 0 {
		return money.Zero(fee.Currency), nil
	}
	if part.AmountMinor > total.AmountMinor {
		part = total
	}
	num := new(big.Int).Mul(big.NewInt(fee.AmountMinor), big.NewInt(part.AmountMinor))
	den := big.NewInt(total.AmountMinor)
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	// half-up：2r >= den 進位。
	if new(big.Int).Mul(r, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return money.Money{}, money.ErrOverflow
	}
	return money.Money{AmountMinor: q.Int64(), Currency: fee.Currency}, nil
}

// Policy 為記帳時的業務政策旗標（docs/02 §7.3 的政策欄位）。
type Policy struct {
	// RefundChargebackFeeOnWin 為 true 時，dispute.won 會加記 J-CB-WON-FEE（退回拒付費）。預設 false。
	RefundChargebackFeeOnWin bool
}

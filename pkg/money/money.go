// Package money 提供金額值物件。
//
// 規則（docs/01-architecture.md §5.1、docs/02 §0.1）：
//   - 金額一律為該幣別最小單位的 int64，禁止浮點數。
//   - 不同幣別相加回傳 ErrCurrencyMismatch（不 panic）。
//   - 百分比以 basis point（1 bps = 0.01%）整數表示，四捨五入為 half-up。
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
)

// 套件錯誤。
var (
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrOverflow         = errors.New("money: int64 overflow")
	ErrNegativeAmount   = errors.New("money: amount must not be negative")
	ErrInvalidCurrency  = errors.New("money: invalid ISO 4217 currency code")
)

// Money 代表某一幣別的一筆金額（最小貨幣單位）。零值（AmountMinor=0, Currency=""）視為「未設定」。
type Money struct {
	AmountMinor int64
	Currency    string
}

// New 建立並驗證金額（非負、幣別合法）。
func New(amountMinor int64, currency string) (Money, error) {
	m := Money{AmountMinor: amountMinor, Currency: currency}
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	return m, nil
}

// MustNew 為測試／常數用，驗證失敗時 panic。
func MustNew(amountMinor int64, currency string) Money {
	m, err := New(amountMinor, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// Zero 回傳該幣別的零金額。
func Zero(currency string) Money { return Money{AmountMinor: 0, Currency: currency} }

// Validate 檢查幣別為 ISO 4217 三碼大寫且在支援表中、金額非負。
func (m Money) Validate() error {
	if !IsSupportedCurrency(m.Currency) {
		return fmt.Errorf("%w: %q", ErrInvalidCurrency, m.Currency)
	}
	if m.AmountMinor < 0 {
		return fmt.Errorf("%w: %d", ErrNegativeAmount, m.AmountMinor)
	}
	return nil
}

// IsZero 判斷金額是否為 0。
func (m Money) IsZero() bool { return m.AmountMinor == 0 }

// IsPositive 判斷金額是否大於 0。
func (m Money) IsPositive() bool { return m.AmountMinor > 0 }

// Add 相加；幣別不同回 ErrCurrencyMismatch，溢位回 ErrOverflow。
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	sum := m.AmountMinor + o.AmountMinor
	// 同號相加才可能溢位：正+正變負或負+負變正。
	if (o.AmountMinor > 0 && sum < m.AmountMinor) || (o.AmountMinor < 0 && sum > m.AmountMinor) {
		return Money{}, ErrOverflow
	}
	return Money{AmountMinor: sum, Currency: m.Currency}, nil
}

// Sub 相減；結果為負回 ErrNegativeAmount（金額值物件不表達方向）。
func (m Money) Sub(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	if o.AmountMinor > m.AmountMinor {
		return Money{}, fmt.Errorf("%w: %d - %d", ErrNegativeAmount, m.AmountMinor, o.AmountMinor)
	}
	return Money{AmountMinor: m.AmountMinor - o.AmountMinor, Currency: m.Currency}, nil
}

// MulBps 以 basis point 計算比例金額（例如手續費 290 bps = 2.9%），half-up 四捨五入到最小單位。
func (m Money) MulBps(bps int64) (Money, error) {
	if bps < 0 {
		return Money{}, fmt.Errorf("%w: bps %d", ErrNegativeAmount, bps)
	}
	if m.AmountMinor == 0 || bps == 0 {
		return Zero(m.Currency), nil
	}
	// 先檢查乘法溢位。
	if m.AmountMinor > math.MaxInt64/bps {
		return Money{}, ErrOverflow
	}
	prod := m.AmountMinor * bps
	q := prod / 10000
	r := prod % 10000
	if r*2 >= 10000 {
		q++
	}
	return Money{AmountMinor: q, Currency: m.Currency}, nil
}

// Cmp 比較金額：-1 / 0 / +1；幣別不同回 ErrCurrencyMismatch。
func (m Money) Cmp(o Money) (int, error) {
	if m.Currency != o.Currency {
		return 0, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	switch {
	case m.AmountMinor < o.AmountMinor:
		return -1, nil
	case m.AmountMinor > o.AmountMinor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal 判斷幣別與金額皆相同。
func (m Money) Equal(o Money) bool {
	return m.Currency == o.Currency && m.AmountMinor == o.AmountMinor
}

// GreaterThan 回傳 m > o（幣別不同視為 false）。
func (m Money) GreaterThan(o Money) bool {
	c, err := m.Cmp(o)
	return err == nil && c > 0
}

// String 以「1234 TWD」格式輸出（僅供 log，不做小數格式化）。
func (m Money) String() string {
	return strconv.FormatInt(m.AmountMinor, 10) + " " + m.Currency
}

// ToProto 轉成 pg.common.v1.Money。
func (m Money) ToProto() *commonv1.Money {
	return &commonv1.Money{AmountMinor: m.AmountMinor, Currency: m.Currency}
}

// FromProto 由 pg.common.v1.Money 轉回並驗證；nil 回傳 ErrInvalidCurrency。
func FromProto(p *commonv1.Money) (Money, error) {
	if p == nil {
		return Money{}, fmt.Errorf("%w: nil money", ErrInvalidCurrency)
	}
	return New(p.GetAmountMinor(), p.GetCurrency())
}

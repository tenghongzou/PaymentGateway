package domain

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// Format 為結算檔格式識別（provider 專屬 parser）。
type Format string

// 支援的格式。
const (
	// FormatMockCSV 為 provider-mock 產出的 CSV：type,provider_reference,amount_minor,currency,fee_minor,settled_at。
	FormatMockCSV Format = "mock_csv"
	// FormatStripeBalanceCSV 為 Stripe Balance Transactions 報表 CSV（欄位子集）。
	FormatStripeBalanceCSV Format = "stripe_balance_csv"
)

// Parser 把結算檔解析成正規化的結算列。
//
// 回傳的 SettlementLine 已填 LineNo / Provider / Type / ProviderReference / Amount / Fee / SettledAt / Raw；
// ID、FileID 由呼叫端（use case）指定。任何一列格式錯誤即整份失敗並回傳 *ParseError（包在 ErrParse 內）。
// 沒有任何資料列時回傳 ErrEmptyFile。
type Parser interface {
	// Format 回傳此 parser 處理的格式。
	Format() Format
	// Provider 回傳此 parser 對應的 PSP 識別碼（寫入 settlement_lines.provider）。
	Provider() string
	// Parse 解析整份檔案。
	Parse(r io.Reader) ([]SettlementLine, error)
}

// ParserFor 依格式回傳 parser；不支援時回傳 ErrUnknownFormat。
func ParserFor(format Format) (Parser, error) {
	switch format {
	case FormatMockCSV:
		return NewMockParser(), nil
	case FormatStripeBalanceCSV:
		return NewStripeParser(), nil
	default:
		return nil, ErrUnknownFormat.WithMessage("Unsupported settlement file format %q.", string(format))
	}
}

// parseMinor 解析「最小單位整數」欄位（不允許小數、負號）。
func parseMinor(line int, field, s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, newParseError(line, field, "empty amount")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, newParseError(line, field, "not an integer minor-unit amount: %q", s)
	}
	if v < 0 {
		return 0, newParseError(line, field, "amount must not be negative: %d", v)
	}
	return v, nil
}

// parseDecimalToMinor 把「主單位小數字串」（例：USD "12.34"、JPY "1200"）轉成最小單位整數。
//
// 規則：小數位數不得超過幣別 exponent（JPY "10.5" 為錯誤）；負號代表方向，取絕對值並回傳 negative=true；
// 全程以整數運算，絕不經過浮點數。
func parseDecimalToMinor(line int, field, s, currency string) (minor int64, negative bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, newParseError(line, field, "empty amount")
	}
	exp := money.Exponent(currency)
	if exp < 0 {
		return 0, false, newParseError(line, field, "unsupported currency %q", currency)
	}
	if strings.HasPrefix(s, "-") {
		negative = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	s = strings.ReplaceAll(s, ",", "")
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if intPart == "" && fracPart == "" {
		return 0, false, newParseError(line, field, "invalid amount %q", s)
	}
	if intPart == "" {
		intPart = "0"
	}
	if hasDot && len(fracPart) > exp {
		return 0, false, newParseError(line, field, "amount %q has more than %d decimal places for %s", s, exp, currency)
	}
	for len(fracPart) < exp {
		fracPart += "0"
	}
	digits := intPart + fracPart
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false, newParseError(line, field, "invalid amount %q", s)
		}
	}
	v, perr := strconv.ParseInt(digits, 10, 64)
	if perr != nil {
		return 0, false, newParseError(line, field, "amount %q overflows int64", s)
	}
	return v, negative, nil
}

// normalizeCurrency 驗證並正規化幣別（大寫三碼、在 pkg/money 支援表內）。
func normalizeCurrency(line int, field, s string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(s))
	if !money.IsSupportedCurrency(c) {
		return "", newParseError(line, field, "unsupported currency %q", s)
	}
	return c, nil
}

// requireHeader 檢查表頭是否包含所有必要欄位，回傳欄名 → 欄位索引。
func requireHeader(header []string, required []string) (map[string]int, error) {
	idx := make(map[string]int, len(header))
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	var missing []string
	for _, r := range required {
		if _, ok := idx[r]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("header missing required columns: %s", strings.Join(missing, ", "))
	}
	return idx, nil
}

// isEmptyRecord 判斷 CSV 列是否全空（略過空白列）。
func isEmptyRecord(rec []string) bool {
	for _, f := range rec {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}

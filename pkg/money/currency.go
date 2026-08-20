package money

// exponents 為支援幣別的小數位數表（ISO 4217 exponent）。
// 未列出的幣別視為不支援（Validate 回 ErrInvalidCurrency）；新增幣別請同時補測試。
var exponents = map[string]int{
	// 0 位小數
	"TWD": 0, "JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "UGX": 0, "XAF": 0, "XOF": 0,
	// 2 位小數
	"USD": 2, "EUR": 2, "GBP": 2, "SGD": 2, "HKD": 2, "CNY": 2, "AUD": 2, "CAD": 2, "NZD": 2,
	"CHF": 2, "SEK": 2, "NOK": 2, "DKK": 2, "THB": 2, "MYR": 2, "PHP": 2, "IDR": 2, "INR": 2,
	"MXN": 2, "BRL": 2, "ZAR": 2, "PLN": 2, "CZK": 2, "HUF": 2, "TRY": 2, "ILS": 2, "AED": 2, "SAR": 2,
	// 3 位小數
	"KWD": 3, "BHD": 3, "JOD": 3, "OMR": 3, "TND": 3, "IQD": 3, "LYD": 3,
}

// Exponent 回傳幣別小數位數；不支援的幣別回傳 -1。
func Exponent(currency string) int {
	if e, ok := exponents[currency]; ok {
		return e
	}
	return -1
}

// IsSupportedCurrency 檢查幣別是否為三碼大寫且在支援表中。
func IsSupportedCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for i := range 3 {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return false
		}
	}
	_, ok := exponents[currency]
	return ok
}

// SupportedCurrencies 回傳支援幣別清單（順序不保證）。
func SupportedCurrencies() []string {
	out := make([]string, 0, len(exponents))
	for c := range exponents {
		out = append(out, c)
	}
	return out
}

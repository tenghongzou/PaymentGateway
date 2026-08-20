package domain

import (
	"crypto/rand"
	"math/big"
	"strings"
)

// base62Alphabet 為 docs/06 §3.1 的字元集（0-9A-Za-z）。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// secretBytes 為隨機部分的位元組數（32 bytes CSPRNG）。
const secretBytes = 32

// secretBodyLen 為 32 bytes 以 base62 編碼後的固定長度（62^43 > 2^256，不足左補 0）。
const secretBodyLen = 43

var base62Radix = big.NewInt(int64(len(base62Alphabet)))

// encodeBase62 把 raw 視為大端整數編碼成 base62，左補 '0' 到固定長度 n。
func encodeBase62(raw []byte, n int) string {
	num := new(big.Int).SetBytes(raw)
	var sb strings.Builder
	sb.Grow(n)
	digits := make([]byte, 0, n)
	mod := new(big.Int)
	zero := big.NewInt(0)
	for num.Cmp(zero) > 0 {
		num.DivMod(num, base62Radix, mod)
		digits = append(digits, base62Alphabet[mod.Int64()])
	}
	for i := len(digits); i < n; i++ {
		sb.WriteByte('0')
	}
	for i := len(digits) - 1; i >= 0; i-- {
		sb.WriteByte(digits[i])
	}
	return sb.String()
}

// randomBody 產生 32 bytes CSPRNG 並編碼為 43 碼 base62（crypto/rand.Read 自 Go 1.24 起保證不回錯誤）。
func randomBody() string {
	raw := make([]byte, secretBytes)
	_, _ = rand.Read(raw)
	return encodeBase62(raw, secretBodyLen)
}

// isBase62 檢查字串是否只含 base62 字元。
func isBase62(s string) bool {
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(base62Alphabet, rune(s[i])) {
			return false
		}
	}
	return true
}

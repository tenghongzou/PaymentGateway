// Package sig 實作商戶請求簽章與對商戶 webhook 簽章（HMAC-SHA256）。
//
// 請求簽章（權威定義 docs/06 §3.3）canonical string 為四行、以 "\n" 分隔、結尾無換行：
//
//	X-Timestamp + "\n" + METHOD + "\n" + request_target + "\n" + hex(sha256(body))
//
// request_target 為 path 加上原始 query（若有），例如 "/v1/payments?limit=10"。
// X-Signature header 值為 "v1=<hex>"；Sign 回傳含 "v1=" 前綴的完整 header 值。
//
// Webhook 簽章 header：X-PG-Signature: t=<unix ts>,v1=<hex hmac>；
// 簽章內容為 "<ts>.<body>"，secret 輪替期間可同時接受多把。
package sig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// 驗證錯誤。
var (
	ErrSignatureMissing     = errors.New("sig: signature or timestamp missing")
	ErrSignatureInvalid     = errors.New("sig: signature mismatch")
	ErrTimestampInvalid     = errors.New("sig: timestamp is not a unix epoch integer")
	ErrTimestampOutOfWindow = errors.New("sig: timestamp outside allowed window")
)

// DefaultWindow 為允許的時間偏差（±300s）。
const DefaultWindow = 300 * time.Second

// CanonicalString 組出請求簽章的標準字串。
func CanonicalString(ts, method, target string, body []byte) string {
	sum := sha256.Sum256(body)
	var b strings.Builder
	b.Grow(len(ts) + len(method) + len(target) + 64 + 3)
	b.WriteString(ts)
	b.WriteString("\n")
	b.WriteString(strings.ToUpper(method))
	b.WriteString("\n")
	b.WriteString(target)
	b.WriteString("\n")
	b.WriteString(hex.EncodeToString(sum[:]))
	return b.String()
}

// SignatureVersion 為目前的簽章版本前綴。
const SignatureVersion = "v1"

// Sign 計算請求簽章，回傳 X-Signature header 值 "v1=<hex 小寫>"。
func Sign(secret, ts, method, target string, body []byte) string {
	return SignatureVersion + "=" + SignHex(secret, ts, method, target, body)
}

// SignHex 只回傳 hex（不含 "v1=" 前綴）。
func SignHex(secret, ts, method, target string, body []byte) string {
	return hmacHex(secret, CanonicalString(ts, method, target, body))
}

// ParseSignature 解析 "v1=<hex>"；格式不符（缺前綴、非 64 hex）回 ErrSignatureMissing。
func ParseSignature(header string) (string, error) {
	ver, hexSig, ok := strings.Cut(strings.TrimSpace(header), "=")
	if !ok || ver != SignatureVersion || len(hexSig) != 64 {
		return "", ErrSignatureMissing
	}
	if _, err := hex.DecodeString(hexSig); err != nil {
		return "", ErrSignatureMissing
	}
	return strings.ToLower(hexSig), nil
}

// Verify 驗證請求簽章：header 格式（v1=）+ 時間窗（±window）+ 常數時間比對。
func Verify(secret, ts, signature, method, target string, body []byte, now time.Time, window time.Duration) error {
	if ts == "" || signature == "" {
		return ErrSignatureMissing
	}
	got, err := ParseSignature(signature)
	if err != nil {
		return err
	}
	if err := checkTimestamp(ts, now, window); err != nil {
		return err
	}
	expected := SignHex(secret, ts, method, target, body)
	if !hmac.Equal([]byte(got), []byte(expected)) {
		return ErrSignatureInvalid
	}
	return nil
}

// VerifyAny 以多把 secret 嘗試驗證（API signing secret 輪替期間使用）。
func VerifyAny(secrets []string, ts, signature, method, target string, body []byte, now time.Time, window time.Duration) error {
	last := ErrSignatureInvalid
	for _, s := range secrets {
		if s == "" {
			continue
		}
		err := Verify(s, ts, signature, method, target, body, now, window)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrSignatureInvalid) {
			return err
		}
		last = err
	}
	return last
}

// ReplayKey 回傳重放偵測用的簽章片段（hex 前 32 碼），格式不符時為空字串。
func ReplayKey(signature string) string {
	h, err := ParseSignature(signature)
	if err != nil {
		return ""
	}
	return h[:32]
}

// SignWebhook 產生 "t=<ts>,v1=<hex>" 格式的 webhook 簽章 header。
func SignWebhook(secret string, ts int64, body []byte) string {
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hmacHex(secret, strconv.FormatInt(ts, 10)+"."+string(body))
}

// VerifyWebhook 驗證 webhook 簽章；支援多把 secret 與多個 v1 值（輪替期間），時間窗 ±window。
func VerifyWebhook(secrets []string, header string, body []byte, now time.Time, window time.Duration) error {
	ts, sigs, err := ParseWebhookHeader(header)
	if err != nil {
		return err
	}
	if err := checkTimestamp(ts, now, window); err != nil {
		return err
	}
	signed := ts + "." + string(body)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		expected := hmacHex(secret, signed)
		for _, s := range sigs {
			if hmac.Equal([]byte(strings.ToLower(s)), []byte(expected)) {
				return nil
			}
		}
	}
	return ErrSignatureInvalid
}

// ParseWebhookHeader 解析 "t=...,v1=...,v1=..."，回傳 ts 與所有 v1 簽章。
func ParseWebhookHeader(header string) (ts string, sigs []string, err error) {
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == "" || len(sigs) == 0 {
		return "", nil, ErrSignatureMissing
	}
	return ts, sigs, nil
}

func checkTimestamp(ts string, now time.Time, window time.Duration) error {
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrTimestampInvalid
	}
	if window <= 0 {
		window = DefaultWindow
	}
	diff := now.Unix() - sec
	if diff < 0 {
		diff = -diff
	}
	if time.Duration(diff)*time.Second > window {
		return ErrTimestampOutOfWindow
	}
	return nil
}

func hmacHex(secret, msg string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(msg)) // hash.Hash.Write 永不回錯
	return hex.EncodeToString(mac.Sum(nil))
}

package domain

import (
	"strings"

	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

// 對商戶 HTTP 投遞使用的 header 名稱（docs/06 §4.2）。
const (
	HeaderSignature  = "X-PG-Signature"
	HeaderEventID    = "X-PG-Event-Id"
	HeaderEventType  = "X-PG-Event-Type"
	HeaderDeliveryID = "X-PG-Delivery-Id"
	HeaderAttempt    = "X-PG-Attempt"
	UserAgent        = "PaymentGateway-Webhooks/1.0"
	ContentTypeJSON  = "application/json"
)

// Signer 產生 X-PG-Signature。簽章內容為 "<ts>.<raw body>"（pkg/sig.SignWebhook），
// secret 輪替期間每把 secret 各產生一個 v1=，格式：t=<ts>,v1=<hmac>[,v1=<hmac>]。
type Signer struct{}

// Sign 以所有非空 secret 簽章；沒有任何 secret 時回 ErrNoSecrets。
func (Signer) Sign(secrets []string, ts int64, body []byte) (string, error) {
	var header string
	for _, s := range secrets {
		if s == "" {
			continue
		}
		h := sig.SignWebhook(s, ts, body)
		if header == "" {
			header = h
			continue
		}
		// h 形如 "t=<ts>,v1=<hex>"；只取 v1 片段附加。
		if i := strings.Index(h, ",v1="); i >= 0 {
			header += h[i:]
		}
	}
	if header == "" {
		return "", ErrNoSecrets
	}
	return header, nil
}

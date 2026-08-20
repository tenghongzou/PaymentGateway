#!/usr/bin/env bash
# 以 PG_DEV_* 開發憑證對本機 api-gateway 建立一筆 tok_ok 付款並查詢。
#
# 用法：
#   ./scripts/dev-pay.sh                      # 預設 http://localhost:8080、TWD 1000、tok_ok
#   TOKEN=tok_decline_hard ./scripts/dev-pay.sh
#   GATEWAY=http://localhost:8080 AMOUNT=2500 CURRENCY=USD ./scripts/dev-pay.sh
#
# 簽章規則（pkg/sig）：
#   X-Signature = "v1=" + hex(HMAC-SHA256(secret, ts + "\n" + METHOD + "\n" + request_target + "\n" + hex(sha256(body))))
set -euo pipefail

GATEWAY="${GATEWAY:-http://localhost:8080}"
API_KEY="${PG_DEV_API_KEY:-pk_test_dev_0000000000000000}"
SECRET="${PG_DEV_SIGNING_SECRET:-sk_test_dev_secret_change_me}"
TOKEN="${TOKEN:-tok_ok}"
AMOUNT="${AMOUNT:-1000}"
CURRENCY="${CURRENCY:-TWD}"
CAPTURE_METHOD="${CAPTURE_METHOD:-automatic}"

for bin in curl openssl; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing $bin" >&2; exit 1; }
done
JQ="cat"; command -v jq >/dev/null 2>&1 && JQ="jq ."

uuid() { if command -v uuidgen >/dev/null 2>&1; then uuidgen | tr 'A-Z' 'a-z'; else od -x /dev/urandom | head -1 | awk '{OFS="-"; print $2$3,$4,$5,$6,$7$8$9}'; fi; }

# sign METHOD TARGET BODY → 印出 "ts sig"
sign() {
  local method="$1" target="$2" body="$3"
  local ts; ts=$(date +%s)
  local body_hash; body_hash=$(printf '%s' "$body" | openssl dgst -sha256 -hex | sed 's/^.* //')
  local canonical; canonical=$(printf '%s\n%s\n%s\n%s' "$ts" "$method" "$target" "$body_hash")
  local sig; sig=$(printf '%s' "$canonical" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')
  echo "$ts v1=$sig"
}

echo ">> POST /v1/payments (token=$TOKEN, amount=$AMOUNT $CURRENCY, capture_method=$CAPTURE_METHOD)"
BODY=$(printf '{"amount":{"amount_minor":%s,"currency":"%s"},"capture_method":"%s","payment_method":{"type":"card","card":{"token":"%s","token_provider":"mock"}},"customer":{"id":"cus_dev","email":"dev@example.com"},"description":"dev-pay.sh","metadata":{"source":"dev-pay.sh"}}' "$AMOUNT" "$CURRENCY" "$CAPTURE_METHOD" "$TOKEN")
read -r TS SIG < <(sign POST /v1/payments "$BODY")
RESP=$(curl -sS -w '\n%{http_code}' -X POST "$GATEWAY/v1/payments" \
  -H "Authorization: Bearer $API_KEY" \
  -H "X-Timestamp: $TS" -H "X-Signature: $SIG" \
  -H "Idempotency-Key: $(uuid)" \
  -H "Content-Type: application/json" \
  -d "$BODY")
CODE=$(printf '%s' "$RESP" | tail -n1)
PAYLOAD=$(printf '%s' "$RESP" | sed '$d')
echo "HTTP $CODE"
printf '%s\n' "$PAYLOAD" | $JQ
PAY_ID=$(printf '%s' "$PAYLOAD" | sed -n 's/.*"id":"\(pay_[A-Za-z0-9]*\)".*/\1/p' | head -n1)
if [ -z "$PAY_ID" ]; then echo "no payment id in response" >&2; exit 1; fi

echo ">> GET /v1/payments/$PAY_ID"
read -r TS SIG < <(sign GET "/v1/payments/$PAY_ID" "")
curl -sS "$GATEWAY/v1/payments/$PAY_ID" \
  -H "Authorization: Bearer $API_KEY" \
  -H "X-Timestamp: $TS" -H "X-Signature: $SIG" | $JQ

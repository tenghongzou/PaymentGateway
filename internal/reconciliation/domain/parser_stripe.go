package domain

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// StripeProvider 為 Stripe 的識別碼。
const StripeProvider = "stripe"

// stripeColumns 為 Stripe Balance Transactions CSV 的必要欄位子集（欄名小寫比對）。
//
// TODO(phase-1)：Stripe 匯出的完整欄位（"Created (UTC)"、"Available On (UTC)"、"Transfer"、"Customer Email"…）
// 與 Balance Report v2 的欄位差異，待 provider-stripe 實作者提供實際樣本後補齊；目前只支援
// id,type,source,amount,fee,net,currency,created 這組子集，created 接受 RFC 3339 或 "2006-01-02 15:04"。
var stripeColumns = []string{"id", "type", "amount", "fee", "currency"}

// StripeParser 解析 Stripe Balance Transactions 報表（CSV）。
//
// 欄位語意：amount / fee / net 為主單位小數（例 USD "12.34"），依幣別 exponent 轉成最小單位；
// 退款、爭議的 amount 為負數，取絕對值後以 type 表達方向。provider_reference 優先使用 source
// （ch_ / re_ / du_），無則用 balance transaction id（txn_）。
type StripeParser struct{}

// NewStripeParser 建立 StripeParser。
func NewStripeParser() *StripeParser { return &StripeParser{} }

// Format 實作 Parser。
func (*StripeParser) Format() Format { return FormatStripeBalanceCSV }

// Provider 實作 Parser。
func (*StripeParser) Provider() string { return StripeProvider }

// Parse 實作 Parser。
func (p *StripeParser) Parse(r io.Reader) ([]SettlementLine, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, ErrEmptyFile
	}
	if err != nil {
		return nil, WrapParse(fmt.Errorf("read header: %w", err))
	}
	idx, err := requireHeader(header, stripeColumns)
	if err != nil {
		return nil, WrapParse(err)
	}
	// created / available_on 欄名在不同匯出版本間不同，擇一即可。
	createdCol := firstPresent(idx, "created (utc)", "created", "created_utc", "available on (utc)", "available_on")
	if createdCol == "" {
		return nil, WrapParse(errors.New("header missing required columns: created"))
	}
	sourceCol := firstPresent(idx, "source", "source_id")

	var lines []SettlementLine
	lineNo := 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, WrapParse(newParseError(lineNo+1, "", "malformed csv: %v", err))
		}
		if isEmptyRecord(rec) {
			continue
		}
		lineNo++
		if len(rec) < len(header) {
			return nil, WrapParse(newParseError(lineNo, "", "expected %d columns, got %d", len(header), len(rec)))
		}
		get := func(col string) string {
			if col == "" {
				return ""
			}
			i, ok := idx[col]
			if !ok {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		typ, ok := stripeLineType(get("type"), get(sourceCol))
		if !ok {
			// payout / transfer / 其他非交易項目：Phase 0 略過。TODO(phase-1)：payout 列用於 J-STL 的 net_paid 核對。
			continue
		}
		currency, err := normalizeCurrency(lineNo, "currency", get("currency"))
		if err != nil {
			return nil, WrapParse(err)
		}
		amount, _, err := parseDecimalToMinor(lineNo, "amount", get("amount"), currency)
		if err != nil {
			return nil, WrapParse(err)
		}
		var fee int64
		if f := get("fee"); f != "" {
			if fee, _, err = parseDecimalToMinor(lineNo, "fee", f, currency); err != nil {
				return nil, WrapParse(err)
			}
		}
		createdAt, err := parseStripeTime(get(createdCol))
		if err != nil {
			return nil, WrapParse(newParseError(lineNo, createdCol, "%v", err))
		}
		ref := get(sourceCol)
		if ref == "" {
			ref = get("id")
		}
		if ref == "" {
			return nil, WrapParse(newParseError(lineNo, "id", "empty id / source"))
		}
		raw := make(map[string]string, len(header))
		for i, h := range header {
			if i < len(rec) {
				raw[strings.ToLower(strings.TrimSpace(h))] = rec[i]
			}
		}
		lines = append(lines, SettlementLine{
			LineNo:            lineNo,
			Provider:          StripeProvider,
			ProviderReference: ref,
			Type:              typ,
			Amount:            money.Money{AmountMinor: amount, Currency: currency},
			Fee:               money.Money{AmountMinor: fee, Currency: currency},
			SettledAt:         createdAt,
			Raw:               raw,
		})
	}
	if len(lines) == 0 {
		return nil, ErrEmptyFile
	}
	return lines, nil
}

// stripeLineType 把 Stripe balance transaction type 對應成 LineType；不參與對帳的類型回傳 ok=false。
func stripeLineType(t, source string) (LineType, bool) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "charge", "payment":
		return LinePayment, true
	case "refund", "payment_refund":
		return LineRefund, true
	case "dispute", "chargeback":
		return LineChargeback, true
	case "adjustment":
		// 舊版報表的爭議以 adjustment + source=du_ 表示。
		if strings.HasPrefix(source, "du_") || strings.HasPrefix(source, "dp_") {
			return LineChargeback, true
		}
		return LineAdjustment, true
	case "stripe_fee", "application_fee", "fee":
		return LineFee, true
	default:
		return "", false
	}
}

// parseStripeTime 接受 RFC 3339 或 Stripe 匯出常見的 "2006-01-02 15:04" / "2006-01-02 15:04:05"（視為 UTC）。
func parseStripeTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

// firstPresent 回傳第一個存在於 idx 的欄名。
func firstPresent(idx map[string]int, names ...string) string {
	for _, n := range names {
		if _, ok := idx[n]; ok {
			return n
		}
	}
	return ""
}

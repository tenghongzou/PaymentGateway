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

// MockProvider 為 provider-mock 的識別碼（與 internal/providermock.ProviderName 一致）。
const MockProvider = "mock"

// mockColumns 為 mock CSV 的必要欄位。
var mockColumns = []string{"type", "provider_reference", "amount_minor", "currency", "fee_minor", "settled_at"}

// MockParser 解析 provider-mock 的結算檔：
//
//	type,provider_reference,amount_minor,currency,fee_minor,settled_at
//	payment,mock_ch_01J...,1000,TWD,59,2026-08-19T02:15:00Z
//
// 金額欄位已是最小單位整數；type 接受 payment（別名 charge）/ refund / chargeback / fee / adjustment。
type MockParser struct{}

// NewMockParser 建立 MockParser。
func NewMockParser() *MockParser { return &MockParser{} }

// Format 實作 Parser。
func (*MockParser) Format() Format { return FormatMockCSV }

// Provider 實作 Parser。
func (*MockParser) Provider() string { return MockProvider }

// Parse 實作 Parser。
func (p *MockParser) Parse(r io.Reader) ([]SettlementLine, error) {
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
	idx, err := requireHeader(header, mockColumns)
	if err != nil {
		return nil, WrapParse(err)
	}

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
		line, err := p.parseRecord(lineNo, header, idx, rec)
		if err != nil {
			return nil, WrapParse(err)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, ErrEmptyFile
	}
	return lines, nil
}

func (p *MockParser) parseRecord(lineNo int, header []string, idx map[string]int, rec []string) (SettlementLine, error) {
	get := func(col string) string { return strings.TrimSpace(rec[idx[col]]) }

	typ := LineType(strings.ToLower(get("type")))
	if typ == "charge" {
		typ = LinePayment
	}
	if !typ.IsValid() {
		return SettlementLine{}, newParseError(lineNo, "type", "unknown type %q", get("type"))
	}
	ref := get("provider_reference")
	if ref == "" {
		return SettlementLine{}, newParseError(lineNo, "provider_reference", "empty provider_reference")
	}
	currency, err := normalizeCurrency(lineNo, "currency", get("currency"))
	if err != nil {
		return SettlementLine{}, err
	}
	amount, err := parseMinor(lineNo, "amount_minor", get("amount_minor"))
	if err != nil {
		return SettlementLine{}, err
	}
	feeStr := get("fee_minor")
	var fee int64
	if feeStr != "" {
		if fee, err = parseMinor(lineNo, "fee_minor", feeStr); err != nil {
			return SettlementLine{}, err
		}
	}
	settledAt, err := time.Parse(time.RFC3339, get("settled_at"))
	if err != nil {
		return SettlementLine{}, newParseError(lineNo, "settled_at", "not RFC 3339: %q", get("settled_at"))
	}
	raw := make(map[string]string, len(header))
	for i, h := range header {
		if i < len(rec) {
			raw[strings.ToLower(strings.TrimSpace(h))] = rec[i]
		}
	}
	return SettlementLine{
		LineNo:            lineNo,
		Provider:          MockProvider,
		ProviderReference: ref,
		MerchantReference: raw["merchant_reference"],
		Type:              typ,
		Amount:            money.Money{AmountMinor: amount, Currency: currency},
		Fee:               money.Money{AmountMinor: fee, Currency: currency},
		SettledAt:         settledAt.UTC(),
		Raw:               raw,
	}, nil
}

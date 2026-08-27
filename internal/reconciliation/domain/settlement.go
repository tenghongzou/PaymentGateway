package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// LineType 為結算列類型（settlement_lines.type CHECK）。
type LineType string

// 結算列類型全集。
const (
	LinePayment    LineType = "payment"
	LineRefund     LineType = "refund"
	LineChargeback LineType = "chargeback"
	LineFee        LineType = "fee"
	LineAdjustment LineType = "adjustment"
)

// IsValid 檢查是否為合法類型。
func (t LineType) IsValid() bool {
	switch t {
	case LinePayment, LineRefund, LineChargeback, LineFee, LineAdjustment:
		return true
	}
	return false
}

// Matchable 回傳此類型是否參與與內部紀錄的比對（fee / adjustment 為 PSP 單方面項目，只統計不比對）。
func (t LineType) Matchable() bool {
	switch t {
	case LinePayment, LineRefund, LineChargeback:
		return true
	case LineFee, LineAdjustment:
		return false
	}
	return false
}

// FileStatus 為結算檔狀態（settlement_files.status CHECK）。
type FileStatus string

// 結算檔狀態全集。
const (
	FilePending   FileStatus = "pending"
	FileImporting FileStatus = "importing"
	FileImported  FileStatus = "imported"
	FileFailed    FileStatus = "failed"
)

// SettlementFile 為匯入的 PSP 結算檔聚合根（對齊 settlement_files 表）。
type SettlementFile struct {
	ID          uuid.UUID
	Provider    string
	FileName    string
	FileHash    string // sha256 hex，冪等依據
	StorageURI  string
	PeriodStart *time.Time // date（UTC 00:00）
	PeriodEnd   *time.Time
	RowCount    int
	Status      FileStatus
	Error       string
	ImportedAt  *time.Time
	Metadata    map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Version     int
}

// FileHash 計算結算檔內容的 sha256 hex。
func FileHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// NewSettlementFile 建立 importing 狀態的結算檔。
func NewSettlementFile(provider, fileName, fileHash string, periodStart, periodEnd *time.Time, now time.Time) *SettlementFile {
	return &SettlementFile{
		ID:          ids.NewUUID(),
		Provider:    provider,
		FileName:    fileName,
		FileHash:    fileHash,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		Status:      FileImporting,
		Metadata:    map[string]string{},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// MarkImporting 把（先前失敗的）檔案重新標為 importing。
func (f *SettlementFile) MarkImporting(now time.Time) {
	f.Status = FileImporting
	f.Error = ""
	f.UpdatedAt = now
}

// MarkImported 標記解析完成並記錄列數。
func (f *SettlementFile) MarkImported(rowCount int, now time.Time) {
	f.Status = FileImported
	f.RowCount = rowCount
	f.Error = ""
	f.ImportedAt = &now
	f.UpdatedAt = now
}

// MarkFailed 標記匯入失敗（解析錯誤等）。
func (f *SettlementFile) MarkFailed(reason string, now time.Time) {
	f.Status = FileFailed
	f.Error = reason
	f.UpdatedAt = now
}

// SettlementLine 為結算檔中一列正規化後的內容（對齊 settlement_lines 表）。
type SettlementLine struct {
	ID                uuid.UUID
	FileID            uuid.UUID
	LineNo            int // 資料列號，1 起算（不含表頭）
	Provider          string
	ProviderReference string
	MerchantReference string
	Type              LineType
	Amount            money.Money // 毛額；方向由 Type 決定
	Fee               money.Money // PSP 手續費（與 Amount 同幣別；無則 0）
	SettledAt         time.Time
	Raw               map[string]string // 原始欄位，供人工檢視
	CreatedAt         time.Time
}

// Net 回傳扣除手續費後的淨額（手續費大於毛額時回傳 0 金額，不會變負數）。
func (l SettlementLine) Net() money.Money {
	net, err := l.Amount.Sub(l.Fee)
	if err != nil {
		return money.Zero(l.Amount.Currency)
	}
	return net
}

// MatchKey 為比對鍵：(provider_reference, type)。
type MatchKey struct {
	ProviderReference string
	Type              LineType
}

// Key 回傳結算列的比對鍵。
func (l SettlementLine) Key() MatchKey {
	return MatchKey{ProviderReference: l.ProviderReference, Type: l.Type}
}

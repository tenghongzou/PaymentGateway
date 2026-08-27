package grpc

import (
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	reconciliationv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/reconciliation/v1"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// formatFor 依 provider + proto 格式決定 domain.Format。
func formatFor(provider string, f reconciliationv1.SettlementFileFormat) (domain.Format, error) {
	switch provider {
	case domain.MockProvider:
		if f == reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_CSV || f == reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_UNSPECIFIED {
			return domain.FormatMockCSV, nil
		}
	case domain.StripeProvider:
		if f == reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_STRIPE_BALANCE_REPORT || f == reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_CSV || f == reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_UNSPECIFIED {
			return domain.FormatStripeBalanceCSV, nil
		}
	case "":
		return "", apperr.ErrParameterMissing.WithMessage("provider is required.").WithParam("provider")
	}
	return "", domain.ErrUnknownFormat.WithMessage("Unsupported provider/format combination: %s/%s.", provider, f.String())
}

func formatToProto(f string) reconciliationv1.SettlementFileFormat {
	switch domain.Format(f) {
	case domain.FormatMockCSV:
		return reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_CSV
	case domain.FormatStripeBalanceCSV:
		return reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_STRIPE_BALANCE_REPORT
	}
	return reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_UNSPECIFIED
}

func runStatusToProto(s domain.RunStatus) reconciliationv1.RunStatus {
	switch s {
	case domain.RunPending:
		return reconciliationv1.RunStatus_RUN_STATUS_PENDING
	case domain.RunRunning:
		return reconciliationv1.RunStatus_RUN_STATUS_RUNNING
	case domain.RunCompleted:
		return reconciliationv1.RunStatus_RUN_STATUS_COMPLETED
	case domain.RunFailed:
		return reconciliationv1.RunStatus_RUN_STATUS_FAILED
	}
	return reconciliationv1.RunStatus_RUN_STATUS_UNSPECIFIED
}

func runStatusFromProto(s reconciliationv1.RunStatus) (domain.RunStatus, bool) {
	switch s {
	case reconciliationv1.RunStatus_RUN_STATUS_PENDING:
		return domain.RunPending, true
	case reconciliationv1.RunStatus_RUN_STATUS_RUNNING:
		return domain.RunRunning, true
	case reconciliationv1.RunStatus_RUN_STATUS_COMPLETED:
		return domain.RunCompleted, true
	case reconciliationv1.RunStatus_RUN_STATUS_FAILED:
		return domain.RunFailed, true
	case reconciliationv1.RunStatus_RUN_STATUS_UNSPECIFIED:
		return "", false
	}
	return "", false
}

// kindToProto 把 domain kind 轉成 proto（amount_mismatch + reason=currency_mismatch → CURRENCY_MISMATCH；duplicate → DUPLICATE_IN_SETTLEMENT）。
func kindToProto(d *domain.Discrepancy) reconciliationv1.DiscrepancyType {
	switch d.Kind {
	case domain.KindMissingInLedger:
		if d.Detail(domain.DetailReason) == "duplicate_in_settlement" {
			return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_DUPLICATE_IN_SETTLEMENT
		}
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_MISSING_IN_LEDGER
	case domain.KindMissingInPSP:
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_MISSING_IN_SETTLEMENT
	case domain.KindAmountMismatch:
		if d.Detail(domain.DetailReason) == "currency_mismatch" {
			return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_CURRENCY_MISMATCH
		}
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_AMOUNT_MISMATCH
	case domain.KindFeeMismatch:
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_FEE_MISMATCH
	case domain.KindStatusMismatch:
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_STATUS_MISMATCH
	}
	return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_UNSPECIFIED
}

// kindFromProto 把 proto 類型轉成 domain kind（CURRENCY / DUPLICATE 以所屬 kind 篩選，再由細節區分）。
func kindFromProto(t reconciliationv1.DiscrepancyType) ([]domain.DiscrepancyKind, error) {
	switch t {
	case reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_MISSING_IN_LEDGER, reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_DUPLICATE_IN_SETTLEMENT:
		return []domain.DiscrepancyKind{domain.KindMissingInLedger}, nil
	case reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_MISSING_IN_SETTLEMENT:
		return []domain.DiscrepancyKind{domain.KindMissingInPSP}, nil
	case reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_AMOUNT_MISMATCH, reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_CURRENCY_MISMATCH:
		return []domain.DiscrepancyKind{domain.KindAmountMismatch}, nil
	case reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_FEE_MISMATCH:
		return []domain.DiscrepancyKind{domain.KindFeeMismatch}, nil
	case reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_STATUS_MISMATCH:
		return []domain.DiscrepancyKind{domain.KindStatusMismatch}, nil
	case reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_UNSPECIFIED:
		// 未指定：與未知值同樣視為無效參數。
	}
	return nil, apperr.ErrParameterInvalid.WithMessage("unsupported discrepancy type %s.", t.String()).WithParam("types")
}

func discrepancyStatusToProto(s domain.DiscrepancyStatus) reconciliationv1.DiscrepancyStatus {
	switch s {
	case domain.DiscrepancyOpen:
		return reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_OPEN
	case domain.DiscrepancyResolved:
		return reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_RESOLVED
	case domain.DiscrepancyIgnored:
		return reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_IGNORED
	}
	return reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_UNSPECIFIED
}

func discrepancyStatusFromProto(s reconciliationv1.DiscrepancyStatus) (domain.DiscrepancyStatus, bool) {
	switch s {
	case reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_OPEN, reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_INVESTIGATING:
		return domain.DiscrepancyOpen, true
	case reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_RESOLVED:
		return domain.DiscrepancyResolved, true
	case reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_IGNORED, reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_AUTO_CLOSED:
		return domain.DiscrepancyIgnored, true
	case reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_UNSPECIFIED:
		return "", false
	}
	return "", false
}

func runToProto(r *domain.Run) *reconciliationv1.ReconciliationRun {
	if r == nil {
		return nil
	}
	s := r.Summary
	byType := map[string]int64{}
	for k, v := range s.ByKind {
		byType[kindNameForProto(domain.DiscrepancyKind(k))] = int64(v)
	}
	return &reconciliationv1.ReconciliationRun{
		Id:             r.PublicID,
		Provider:       r.Provider,
		SettlementDate: r.SettlementDate(),
		FileName:       s.FileName,
		FileFormat:     formatToProto(s.FileFormat),
		FileChecksum:   s.FileHash,
		FileSizeBytes:  s.FileSizeBytes,
		Status:         runStatusToProto(r.Status),
		Summary: &reconciliationv1.RunSummary{
			TotalRecords:        int64(s.TotalLines),
			Matched:             int64(s.Matched),
			Discrepancies:       int64(s.Unmatched),
			DiscrepanciesByType: byType,
			TotalSettled:        moniesToProto(s.TotalSettled),
			TotalFees:           moniesToProto(s.TotalFees),
			TotalRefunds:        moniesToProto(s.TotalRefunds),
			TotalChargebacks:    moniesToProto(s.TotalChargebacks),
		},
		ErrorMessage: r.Error,
		TriggeredBy:  r.TriggeredBy,
		CreatedAt:    timestamppb.New(r.CreatedAt),
		StartedAt:    tsOrNil(r.StartedAt),
		FinishedAt:   tsOrNil(r.FinishedAt),
	}
}

// kindNameForProto 回傳 DiscrepancyType 的名稱（summary.discrepancies_by_type 的 key）。
func kindNameForProto(k domain.DiscrepancyKind) string {
	switch k {
	case domain.KindMissingInLedger:
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_MISSING_IN_LEDGER.String()
	case domain.KindMissingInPSP:
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_MISSING_IN_SETTLEMENT.String()
	case domain.KindAmountMismatch:
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_AMOUNT_MISMATCH.String()
	case domain.KindFeeMismatch:
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_FEE_MISMATCH.String()
	case domain.KindStatusMismatch:
		return reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_STATUS_MISMATCH.String()
	}
	return string(k)
}

func discrepancyToProto(d *domain.Discrepancy) *reconciliationv1.Discrepancy {
	if d == nil {
		return nil
	}
	out := &reconciliationv1.Discrepancy{
		Id:                d.PublicID(),
		RunId:             ids.Format(domain.PrefixRun, d.RunID),
		Provider:          d.Provider,
		Type:              kindToProto(d),
		Status:            discrepancyStatusToProto(d.Status),
		ProviderReference: d.ProviderReference,
		ExpectedStatus:    d.Detail(domain.DetailExpectedStatus),
		ActualStatus:      d.Detail(domain.DetailActualStatus),
		Description:       d.Detail("description"),
		ResolutionNote:    d.ResolutionNote,
		ResolvedBy:        d.ResolvedBy,
		CreatedAt:         timestamppb.New(d.CreatedAt),
		ResolvedAt:        tsOrNil(d.ResolvedAt),
	}
	if d.MerchantID != nil {
		out.MerchantId = ids.Format(ids.PrefixMerchant, *d.MerchantID)
	}
	switch {
	case strings.HasPrefix(d.InternalReference, ids.PrefixPayment+"_"):
		out.PaymentId = d.InternalReference
	case strings.HasPrefix(d.InternalReference, ids.PrefixRefund+"_"):
		out.RefundId = d.InternalReference
	case strings.HasPrefix(d.InternalReference, ids.PrefixDispute+"_"):
		out.DisputeId = d.InternalReference
	default:
		out.PaymentId = d.InternalReference
	}
	if d.ExpectedAmount != nil {
		out.ExpectedAmount = &commonv1.Money{AmountMinor: *d.ExpectedAmount, Currency: currencyOr(d.Detail("expected_currency"), d.Currency)}
	}
	if d.ActualAmount != nil {
		out.ActualAmount = &commonv1.Money{AmountMinor: *d.ActualAmount, Currency: d.Currency}
	}
	if d.ExpectedAmount != nil && d.ActualAmount != nil && d.Detail(domain.DetailReason) != "currency_mismatch" {
		// proto 註解：difference = actual - expected，可為負；Money.amount_minor 以帶號值表達（僅此欄位）。
		out.Difference = &commonv1.Money{AmountMinor: *d.ActualAmount - *d.ExpectedAmount, Currency: d.Currency}
	}
	switch d.Status {
	case domain.DiscrepancyResolved:
		out.ResolutionAction = reconciliationv1.ResolutionAction_RESOLUTION_ACTION_MARK_RESOLVED
	case domain.DiscrepancyIgnored:
		out.ResolutionAction = reconciliationv1.ResolutionAction_RESOLUTION_ACTION_IGNORE
	case domain.DiscrepancyOpen:
		// open 尚未處理：resolution_action 維持 UNSPECIFIED。
	}
	if line, ok := d.LineSnapshot(); ok {
		out.SettlementRecord = &reconciliationv1.SettlementRecord{
			LineNumber:        int64(line.LineNo),
			ProviderReference: line.ProviderReference,
			RecordType:        string(line.Type),
			GrossAmount:       &commonv1.Money{AmountMinor: line.Amount.AmountMinor, Currency: line.Amount.Currency},
			Fee:               &commonv1.Money{AmountMinor: line.Fee.AmountMinor, Currency: line.Amount.Currency},
			NetAmount:         &commonv1.Money{AmountMinor: line.Net().AmountMinor, Currency: line.Amount.Currency},
			SettledAt:         timestamppb.New(line.SettledAt),
		}
	}
	return out
}

func currencyOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func moniesToProto(m map[string]int64) []*commonv1.Money {
	if len(m) == 0 {
		return nil
	}
	out := make([]*commonv1.Money, 0, len(m))
	for cur, amt := range m {
		out = append(out, &commonv1.Money{AmountMinor: amt, Currency: cur})
	}
	return out
}

func tsOrNil(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func pageResponse(next string) *commonv1.PageResponse {
	return &commonv1.PageResponse{NextPageToken: next, HasMore: next != ""}
}

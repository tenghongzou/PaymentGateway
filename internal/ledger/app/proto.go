package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	ledgerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/ledger/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// 本檔案集中 domain ↔ protobuf（pg.ledger.v1 / pg.payment.v1）的轉換，供 gRPC adapter、outbox payload 與 consumer 共用。

// --- ID 轉換 ---

// AccountPublicID 回傳 acct_ 公開 ID。
func AccountPublicID(id uuid.UUID) string { return ids.Format(ids.PrefixAccount, id) }

// MerchantPublicID 回傳 mch_ 公開 ID（系統帳戶 / 系統 journal 為空字串）。
func MerchantPublicID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return ids.Format(ids.PrefixMerchant, id)
}

// ParseAccountID 解析 acct_ 公開 ID（也接受裸 uuid，方便內部工具）。
func ParseAccountID(s string) (uuid.UUID, error) {
	return parsePrefixed(s, ids.PrefixAccount, "account_id")
}

// ParseJournalID 解析 jrn_ 公開 ID。
func ParseJournalID(s string) (uuid.UUID, error) {
	return parsePrefixed(s, ids.PrefixJournal, "journal_id")
}

// ParseMerchantID 解析 mch_ 公開 ID。
func ParseMerchantID(s string) (uuid.UUID, error) {
	return parsePrefixed(s, ids.PrefixMerchant, "merchant_id")
}

// ParseEventID 解析 evt_ 公開 ID（也接受裸 uuid：outbox.id 即 uuid）。
func ParseEventID(s string) (uuid.UUID, error) {
	return parsePrefixed(s, ids.PrefixEvent, "event_id")
}

func parsePrefixed(s, prefix, param string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, apperr.ErrParameterMissing.WithParam(param).WithMessage("%s is required", param)
	}
	if u, err := ids.ParseWithPrefix(s, prefix); err == nil {
		return u, nil
	}
	if u, err := uuid.Parse(s); err == nil {
		return u, nil
	}
	return uuid.Nil, apperr.ErrParameterInvalid.WithParam(param).WithMessage("%s %q is not a valid %s_ id", param, s, prefix)
}

// IdempotencyKeyToEventID 把任意 idempotency_key 轉成 journals.event_id（uuid）：
// evt_ / uuid 直接使用；其他字串以 UUIDv5（固定 namespace）做確定性映射。
func IdempotencyKeyToEventID(key string) (uuid.UUID, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return uuid.Nil, domain.ErrEventIDMissing
	}
	if u, err := ids.ParseWithPrefix(key, ids.PrefixEvent); err == nil {
		return u, nil
	}
	if u, err := uuid.Parse(key); err == nil {
		return u, nil
	}
	return uuid.NewSHA1(idempotencyNamespace, []byte(key)), nil
}

// idempotencyNamespace 為 UUIDv5 的固定命名空間（ledger-service idempotency）。
var idempotencyNamespace = uuid.MustParse("6f1c9d2e-4b7a-5c3d-9e8f-1a2b3c4d5e6f")

// --- enum 轉換 ---

var kindToProto = map[domain.Kind]ledgerv1.AccountType{
	domain.KindMerchantPayable:   ledgerv1.AccountType_ACCOUNT_TYPE_MERCHANT_PAYABLE,
	domain.KindPSPReceivable:     ledgerv1.AccountType_ACCOUNT_TYPE_PSP_RECEIVABLE,
	domain.KindFeeRevenue:        ledgerv1.AccountType_ACCOUNT_TYPE_FEE_REVENUE,
	domain.KindRefundClearing:    ledgerv1.AccountType_ACCOUNT_TYPE_REFUND_CLEARING,
	domain.KindChargebackReserve: ledgerv1.AccountType_ACCOUNT_TYPE_CHARGEBACK_RESERVE,
}

// KindToProto 把科目種類轉成 proto AccountType；proto 未列出的配套科目回 UNSPECIFIED（name 仍帶完整 code）。
func KindToProto(k domain.Kind) ledgerv1.AccountType {
	if v, ok := kindToProto[k]; ok {
		return v
	}
	return ledgerv1.AccountType_ACCOUNT_TYPE_UNSPECIFIED
}

// KindFromProto 把 proto AccountType 轉成科目種類。
func KindFromProto(t ledgerv1.AccountType) (domain.Kind, error) {
	for k, v := range kindToProto {
		if v == t {
			return k, nil
		}
	}
	return "", apperr.ErrParameterInvalid.WithParam("type").WithMessage("account type %s is not supported", t)
}

// DirectionFromProto 轉換分錄方向。
func DirectionFromProto(d ledgerv1.EntryDirection) (domain.Direction, error) {
	switch d {
	case ledgerv1.EntryDirection_ENTRY_DIRECTION_DEBIT:
		return domain.Debit, nil
	case ledgerv1.EntryDirection_ENTRY_DIRECTION_CREDIT:
		return domain.Credit, nil
	default:
		return "", domain.ErrEntryDirectionInvalid
	}
}

// DirectionToProto 轉換分錄方向。
func DirectionToProto(d domain.Direction) ledgerv1.EntryDirection {
	switch d {
	case domain.Debit:
		return ledgerv1.EntryDirection_ENTRY_DIRECTION_DEBIT
	case domain.Credit:
		return ledgerv1.EntryDirection_ENTRY_DIRECTION_CREDIT
	default:
		return ledgerv1.EntryDirection_ENTRY_DIRECTION_UNSPECIFIED
	}
}

var sourceToProto = map[domain.SourceType]ledgerv1.JournalSourceType{
	domain.SourcePaymentEvent:             ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_PAYMENT_EVENT,
	domain.SourceReconciliationAdjustment: ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_RECONCILIATION_ADJUSTMENT,
	domain.SourceManualAdjustment:         ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_MANUAL_ADJUSTMENT,
	domain.SourceReversal:                 ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_REVERSAL,
	domain.SourcePayout:                   ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_PAYOUT,
}

// SourceTypeToProto 轉換來源類型。
func SourceTypeToProto(s domain.SourceType) ledgerv1.JournalSourceType {
	return sourceToProto[s]
}

// SourceTypeFromProto 轉換來源類型；UNSPECIFIED 回錯誤。
func SourceTypeFromProto(s ledgerv1.JournalSourceType) (domain.SourceType, error) {
	for k, v := range sourceToProto {
		if v == s {
			return k, nil
		}
	}
	return "", apperr.ErrParameterInvalid.WithParam("source_type").WithMessage("source_type is required")
}

// StatusToProto 轉換帳戶狀態（closed 對外也視為 FROZEN：皆不可過帳）。
func StatusToProto(s domain.AccountStatus) ledgerv1.AccountStatus {
	switch s {
	case domain.AccountActive:
		return ledgerv1.AccountStatus_ACCOUNT_STATUS_ACTIVE
	case domain.AccountFrozen, domain.AccountClosed:
		return ledgerv1.AccountStatus_ACCOUNT_STATUS_FROZEN
	default:
		return ledgerv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED
	}
}

// --- message 轉換 ---

// ToProtoAccount 轉換帳戶。
func ToProtoAccount(a *domain.Account) *ledgerv1.Account {
	return &ledgerv1.Account{
		Id:         AccountPublicID(a.ID),
		Type:       KindToProto(a.Kind()),
		MerchantId: MerchantPublicID(a.Key.MerchantID),
		Provider:   a.Key.Qualifier(),
		Currency:   a.Key.Currency,
		Name:       a.Name,
		Status:     StatusToProto(a.Status),
		Livemode:   a.Key.Livemode,
		CreatedAt:  tsOrNil(a.CreatedAt),
	}
}

// ToProtoEntry 轉換分錄。
func ToProtoEntry(journalPublicID string, e domain.Entry) *ledgerv1.Entry {
	return &ledgerv1.Entry{
		Id:          ids.Format(ids.PrefixEntry, e.ID),
		JournalId:   journalPublicID,
		AccountId:   AccountPublicID(e.AccountID),
		AccountType: KindToProto(e.Account.Kind()),
		Direction:   DirectionToProto(e.Direction),
		Amount:      e.Amount.ToProto(),
		Description: e.Description,
	}
}

// ToProtoJournal 轉換 journal（含 entries）。
func ToProtoJournal(j *domain.Journal) *ledgerv1.Journal {
	out := &ledgerv1.Journal{
		Id:             j.PublicID,
		MerchantId:     MerchantPublicID(j.MerchantID),
		SourceType:     SourceTypeToProto(j.SourceType),
		SourceId:       j.SourceID,
		ReferenceType:  string(j.ReferenceType),
		ReferenceId:    j.ReferenceID,
		Description:    j.Description,
		Currency:       j.Currency(),
		IdempotencyKey: j.EventID.String(),
		Livemode:       j.Livemode,
		Metadata:       publicMetadata(j),
		PostedAt:       tsOrNil(j.PostedAt),
		EffectiveAt:    tsOrNil(j.EffectiveAt),
	}
	if j.ReversedBy != nil {
		out.ReversedByJournalId = ids.Format(ids.PrefixJournal, *j.ReversedBy)
	}
	for _, e := range j.Entries {
		out.Entries = append(out.Entries, ToProtoEntry(j.PublicID, e))
	}
	return out
}

// publicMetadata 回傳對外的 metadata（含系統保留鍵 template / payment_id 等，方便下游關聯）。
func publicMetadata(j *domain.Journal) map[string]string {
	out := make(map[string]string, len(j.Metadata)+2)
	for k, v := range j.Metadata {
		out[k] = v
	}
	if j.Template != "" {
		out[domain.MetaTemplate] = j.Template
	}
	if j.ReversalOf != nil {
		out["reversal_of_journal_id"] = ids.Format(ids.PrefixJournal, *j.ReversalOf)
	}
	return out
}

// ToProtoBalance 轉換帳戶餘額。
func ToProtoBalance(b *domain.Balance) *ledgerv1.Balance {
	out := &ledgerv1.Balance{
		AccountId:        AccountPublicID(b.AccountID),
		AccountType:      KindToProto(b.Account.Kind()),
		Currency:         b.Account.Currency,
		BalanceMinor:     b.Balance,
		TotalDebitMinor:  b.TotalDebit,
		TotalCreditMinor: b.TotalCredit,
		AsOf:             tsOrNil(b.UpdatedAt),
	}
	if b.AsOfJournalID != nil {
		out.AsOfJournalId = ids.Format(ids.PrefixJournal, *b.AsOfJournalID)
	}
	return out
}

// ToProtoMerchantBalance 轉換商戶餘額拆解。
func ToProtoMerchantBalance(b domain.MerchantBalance) *ledgerv1.MerchantBalance {
	return &ledgerv1.MerchantBalance{
		MerchantId:     MerchantPublicID(b.MerchantID),
		Currency:       b.Currency,
		AvailableMinor: b.Available,
		PendingMinor:   b.Pending,
		ReservedMinor:  b.Reserved,
		PayableMinor:   b.Payable,
		AsOf:           tsOrNil(b.AsOf),
	}
}

func tsOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// --- payment.events → domain.PaymentEvent ---

// EventTypeFromProto 把 PaymentEventType 轉成 "payment.captured" 形式（去前綴、小寫、第一個 _ 轉 .）。
func EventTypeFromProto(t paymentv1.PaymentEventType) domain.EventType {
	name := strings.TrimPrefix(t.String(), "PAYMENT_EVENT_TYPE_")
	name = strings.ToLower(name)
	return domain.EventType(strings.Replace(name, "_", ".", 1))
}

// FromProtoPaymentEvent 把 Kafka 的 PaymentEvent 轉成 domain 事件。eventID 為 header / 信封解析後的 uuid。
func FromProtoPaymentEvent(ev *paymentv1.PaymentEvent, eventID uuid.UUID) (domain.PaymentEvent, error) {
	if ev == nil {
		return domain.PaymentEvent{}, domain.ErrEventInvalid.WithMessage("nil event")
	}
	out := domain.PaymentEvent{
		EventID:          eventID,
		EventPublicID:    ev.GetEventId(),
		Type:             EventTypeFromProto(ev.GetEventType()),
		MerchantPublicID: ev.GetMerchantId(),
		PaymentID:        ev.GetPaymentId(),
		Livemode:         ev.GetLivemode(),
		PaymentVersion:   ev.GetPaymentVersion(),
	}
	if ev.GetOccurredAt() != nil {
		out.OccurredAt = ev.GetOccurredAt().AsTime()
	}
	if ev.GetMerchantId() != "" {
		m, err := ParseMerchantID(ev.GetMerchantId())
		if err != nil {
			return domain.PaymentEvent{}, domain.ErrEventInvalid.WithMessage("merchant_id: %v", err)
		}
		out.MerchantID = m
	}
	switch p := ev.GetPayload().(type) {
	case *paymentv1.PaymentEvent_PaymentCaptured:
		out.Provider = p.PaymentCaptured.GetProvider()
		if err := setAmounts(&out, p.PaymentCaptured.GetAmount(), p.PaymentCaptured.GetFee()); err != nil {
			return out, err
		}
	case *paymentv1.PaymentEvent_RefundCreated:
		out.Provider = p.RefundCreated.GetProvider()
		out.RefundID = p.RefundCreated.GetRefundId()
		if err := setAmounts(&out, p.RefundCreated.GetAmount(), nil); err != nil {
			return out, err
		}
	case *paymentv1.PaymentEvent_RefundSucceeded:
		out.Provider = p.RefundSucceeded.GetProvider()
		out.RefundID = p.RefundSucceeded.GetRefundId()
		if err := setAmounts(&out, p.RefundSucceeded.GetAmount(), p.RefundSucceeded.GetFee()); err != nil {
			return out, err
		}
	case *paymentv1.PaymentEvent_RefundFailed:
		out.Provider = p.RefundFailed.GetProvider()
		out.RefundID = p.RefundFailed.GetRefundId()
		if err := setAmounts(&out, p.RefundFailed.GetAmount(), nil); err != nil {
			return out, err
		}
	case *paymentv1.PaymentEvent_DisputeOpened:
		out.Provider = p.DisputeOpened.GetProvider()
		out.DisputeID = p.DisputeOpened.GetDisputeId()
		if err := setAmounts(&out, p.DisputeOpened.GetAmount(), p.DisputeOpened.GetFee()); err != nil {
			return out, err
		}
	case *paymentv1.PaymentEvent_DisputeClosed:
		out.Provider = p.DisputeClosed.GetProvider()
		out.DisputeID = p.DisputeClosed.GetDisputeId()
		if err := setAmounts(&out, p.DisputeClosed.GetAmount(), p.DisputeClosed.GetFee()); err != nil {
			return out, err
		}
	case *paymentv1.PaymentEvent_PaymentAuthorized:
		out.Provider = p.PaymentAuthorized.GetProvider()
	case *paymentv1.PaymentEvent_PaymentRequiresAction:
		out.Provider = p.PaymentRequiresAction.GetProvider()
	case *paymentv1.PaymentEvent_PaymentVoided:
		out.Provider = p.PaymentVoided.GetProvider()
	case *paymentv1.PaymentEvent_PaymentFailed:
		out.Provider = p.PaymentFailed.GetProvider()
	case *paymentv1.PaymentEvent_PaymentExpired:
		out.Provider = p.PaymentExpired.GetProvider()
	case *paymentv1.PaymentEvent_DisputeEvidenceSubmitted:
		out.Provider = p.DisputeEvidenceSubmitted.GetProvider()
		out.DisputeID = p.DisputeEvidenceSubmitted.GetDisputeId()
	case *paymentv1.PaymentEvent_PaymentCreated, nil:
		// 無 provider / 金額需求
	default:
		return out, domain.ErrEventInvalid.WithMessage("unknown payload %T", p)
	}
	return out, nil
}

// setAmounts 填入 Amount / Fee（fee 為 nil 或 0 時保持零值）。
func setAmounts(out *domain.PaymentEvent, amount, fee *commonv1.Money) error {
	m, err := money.FromProto(amount)
	if err != nil {
		return domain.ErrEventInvalid.WithMessage("amount: %v", err)
	}
	out.Amount = m
	if fee != nil && fee.GetAmountMinor() != 0 {
		f, err := money.FromProto(fee)
		if err != nil {
			return domain.ErrEventInvalid.WithMessage("fee: %v", err)
		}
		out.Fee = f
	}
	return nil
}

// String 供除錯輸出。
func (c Cursor) String() string { return fmt.Sprintf("%s/%s", c.At.Format(time.RFC3339Nano), c.ID) }

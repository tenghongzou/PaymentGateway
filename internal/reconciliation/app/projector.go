package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// ErrPoisonMessage 表示 payload 無法反序列化（消費端應送 DLQ，不再重試）。
type ErrPoisonMessage struct{ Err error }

// Error 實作 error。
func (e *ErrPoisonMessage) Error() string { return "reconciliation: poison message: " + e.Err.Error() }

// Unwrap 讓 errors.Is 能穿透。
func (e *ErrPoisonMessage) Unwrap() error { return e.Err }

// HandlePaymentEvent 消費 payment.events 維護 payment_records 讀模型（docs/05 §8 消費端交易邊界）：
//
//	BEGIN → processed_events 去重 → 反序列化 → upsert（source_seq 丟棄亂序）→ COMMIT
//
// eventID 為 Kafka header event_id（uuid）；不相關的事件型別只記錄去重並回傳 nil。
func (s *Service) HandlePaymentEvent(ctx context.Context, eventID string, payload []byte) error {
	if eventID == "" {
		return &ErrPoisonMessage{Err: fmt.Errorf("missing event_id")}
	}
	return s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		already, err := s.d.Inbox.MarkProcessed(ctx, eventID, s.cfg.ConsumerName)
		if err != nil {
			return err
		}
		if already {
			s.d.Logger.DebugContext(ctx, "duplicate payment event skipped", "event_id", eventID)
			return nil
		}
		var ev paymentv1.PaymentEvent
		if err = proto.Unmarshal(payload, &ev); err != nil {
			return &ErrPoisonMessage{Err: err}
		}
		rec, ok := ProjectPaymentEvent(&ev, s.d.Clock.Now())
		if !ok {
			return nil
		}
		applied, err := s.d.Records.Upsert(ctx, rec)
		if err != nil {
			return err
		}
		if !applied {
			s.d.Logger.DebugContext(ctx, "stale payment event ignored", "event_id", eventID, "public_id", rec.PublicID, "seq", rec.SourceSeq)
		}
		return nil
	})
}

// ProjectPaymentEvent 把 PaymentEvent 投影成 PaymentRecord；與對帳無關的事件回傳 ok=false。
func ProjectPaymentEvent(ev *paymentv1.PaymentEvent, now time.Time) (*domain.PaymentRecord, bool) {
	occurred := now
	if ts := ev.GetOccurredAt(); ts != nil {
		occurred = ts.AsTime()
	}
	base := domain.PaymentRecord{
		MerchantID: uuidFromPublicID(ev.GetMerchantId()),
		OccurredAt: occurred.UTC(),
		SourceSeq:  int(ev.GetPaymentVersion()),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	payment := func(publicID string) {
		base.Kind = domain.RecordPayment
		base.PublicID = publicID
		base.ID = uuidFromPublicID(publicID)
	}

	switch ev.GetEventType() {
	case paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_AUTHORIZED:
		p := ev.GetPaymentAuthorized()
		payment(ev.GetPaymentId())
		base.Provider, base.ProviderReference = p.GetProvider(), p.GetProviderReference()
		base.Amount, base.Fee = moneyOf(p.GetAmount()), optMoney(p.GetFee())
		base.Status = domain.StatusAuthorized
	case paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CAPTURED:
		p := ev.GetPaymentCaptured()
		payment(ev.GetPaymentId())
		base.Provider, base.ProviderReference = p.GetProvider(), p.GetProviderReference()
		amt := p.GetTotalCapturedAmount()
		if amt == nil {
			amt = p.GetAmount()
		}
		base.Amount, base.Fee = moneyOf(amt), optMoney(p.GetFee())
		base.Status = paymentStatusString(ev.GetPaymentStatus(), domain.StatusCaptured)
	case paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_VOIDED:
		p := ev.GetPaymentVoided()
		payment(ev.GetPaymentId())
		base.Provider, base.ProviderReference = p.GetProvider(), p.GetProviderReference()
		base.Amount = moneyOf(p.GetAmount())
		base.Status = domain.StatusVoided
	case paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_CREATED:
		p := ev.GetRefundCreated()
		base.Kind, base.PublicID, base.ID = domain.RecordRefund, p.GetRefundId(), uuidFromPublicID(p.GetRefundId())
		base.Provider = p.GetProvider()
		base.Amount = moneyOf(p.GetAmount())
		base.Status = domain.RefundPending
	case paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_SUCCEEDED:
		p := ev.GetRefundSucceeded()
		base.Kind, base.PublicID, base.ID = domain.RecordRefund, p.GetRefundId(), uuidFromPublicID(p.GetRefundId())
		base.Provider, base.ProviderReference = p.GetProvider(), p.GetProviderReference()
		base.Amount, base.Fee = moneyOf(p.GetAmount()), optMoney(p.GetFee())
		base.Status = domain.RefundSucceeded
	case paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_REFUND_FAILED:
		p := ev.GetRefundFailed()
		base.Kind, base.PublicID, base.ID = domain.RecordRefund, p.GetRefundId(), uuidFromPublicID(p.GetRefundId())
		base.Provider = p.GetProvider()
		base.Amount = moneyOf(p.GetAmount())
		base.Status = domain.RefundFailed
	case paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_OPENED:
		p := ev.GetDisputeOpened()
		base.Kind, base.PublicID, base.ID = domain.RecordDispute, p.GetDisputeId(), uuidFromPublicID(p.GetDisputeId())
		base.Provider, base.ProviderReference = p.GetProvider(), p.GetProviderReference()
		base.Amount, base.Fee = moneyOf(p.GetAmount()), optMoney(p.GetFee())
		base.Status = domain.DisputeOpen
	case paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_WON, paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_LOST:
		p := ev.GetDisputeClosed()
		base.Kind, base.PublicID, base.ID = domain.RecordDispute, p.GetDisputeId(), uuidFromPublicID(p.GetDisputeId())
		base.Provider, base.ProviderReference = p.GetProvider(), p.GetProviderReference()
		base.Amount, base.Fee = moneyOf(p.GetAmount()), optMoney(p.GetFee())
		base.Status = domain.DisputeLost
		if ev.GetEventType() == paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_WON || p.GetOutcome() == providerv1.DisputeOutcome_DISPUTE_OUTCOME_WON {
			base.Status = domain.DisputeWon
		}
	case paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_UNSPECIFIED,
		paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_CREATED,
		paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_REQUIRES_ACTION,
		paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_FAILED,
		paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_PAYMENT_EXPIRED,
		paymentv1.PaymentEventType_PAYMENT_EVENT_TYPE_DISPUTE_EVIDENCE_SUBMITTED:
		// 未請款 / 未進入結算的事件與對帳讀模型無關：只去重，不投影。
		return nil, false
	default:
		return nil, false
	}
	if base.PublicID == "" || base.Amount.Currency == "" {
		return nil, false
	}
	if base.MerchantID == uuid.Nil {
		base.MerchantID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("merchant:unknown"))
	}
	return &base, true
}

// paymentStatusString 把 proto PaymentStatus 轉成讀模型字串；UNSPECIFIED 時用 fallback。
func paymentStatusString(st paymentv1.PaymentStatus, fallback string) string {
	if st == paymentv1.PaymentStatus_PAYMENT_STATUS_UNSPECIFIED {
		return fallback
	}
	name := strings.TrimPrefix(st.String(), "PAYMENT_STATUS_")
	return strings.ToLower(name)
}

// uuidFromPublicID 把公開 ID（pay_ / re_ / dp_ / mch_ 等，內容為 UUIDv7）還原成 uuid；
// 非標準格式（測試資料）則以 uuid v5 雜湊，保證同一字串映射到同一 uuid。
func uuidFromPublicID(s string) uuid.UUID {
	if s == "" {
		return uuid.Nil
	}
	if _, u, err := ids.Parse(s); err == nil {
		return u
	}
	if u, err := uuid.Parse(s); err == nil {
		return u
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(s))
}

func moneyOf(m *commonv1.Money) money.Money {
	if m == nil {
		return money.Money{}
	}
	return money.Money{AmountMinor: m.GetAmountMinor(), Currency: m.GetCurrency()}
}

func optMoney(m *commonv1.Money) *money.Money {
	if m == nil || m.GetCurrency() == "" {
		return nil
	}
	v := moneyOf(m)
	return &v
}

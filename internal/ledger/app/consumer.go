package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	paymentv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/payment/v1"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/pkg/eventbus"
)

// ErrPoisonMessage 表示訊息無法反序列化 / 缺必要欄位；consumer 重試耗盡後送 DLQ（docs/05 §8.2 第 6 點）。
var ErrPoisonMessage = errors.New("ledger: poison message")

// HandlePaymentEvent 為 payment.events 的 handler（實作 eventbus.Handler 的內容）：
//
//	解析 PaymentEvent protobuf → 同一交易內 Inbox.MarkProcessed 去重 → TemplateFor → postJournalTx。
//
// 無對應範本的事件（授權、失敗、證據提交…）只記 processed_events 後 ack；重複事件直接 ack；
// 其他錯誤回傳給 consumer 重試（最終 DLQ）。
func (s *Service) HandlePaymentEvent(ctx context.Context, rec eventbus.Record) error {
	var ev paymentv1.PaymentEvent
	if err := proto.Unmarshal(rec.Value, &ev); err != nil {
		return fmt.Errorf("%w: unmarshal PaymentEvent (topic=%s offset=%d): %w", ErrPoisonMessage, rec.Topic, rec.Offset, err)
	}
	eventIDStr := rec.EventID()
	if eventIDStr == "" {
		eventIDStr = ev.GetEventId()
	}
	eventID, err := ParseEventID(eventIDStr)
	if err != nil {
		return fmt.Errorf("%w: event_id %q: %w", ErrPoisonMessage, eventIDStr, err)
	}
	dev, err := FromProtoPaymentEvent(&ev, eventID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPoisonMessage, err)
	}
	log := s.log.With("event_id", eventIDStr, "event_type", dev.Type, "payment_id", dev.PaymentID, "merchant_id", dev.MerchantPublicID)

	return s.tx.RunInTx(ctx, func(ctx context.Context) error {
		already, err := s.inbox.MarkProcessed(ctx, eventID, ConsumerPaymentEvents)
		if err != nil {
			return err
		}
		if already {
			log.DebugContext(ctx, "duplicate payment event ignored")
			return nil
		}
		j, err := domain.TemplateFor(dev, s.policy)
		if errors.Is(err, domain.ErrNoTemplate) {
			log.DebugContext(ctx, "payment event does not post to the ledger; acked")
			return nil
		}
		if err != nil {
			// 事件內容不足以記帳（缺 provider / 金額 / 費用超過金額…）：屬 poison，交由 consumer 重試→DLQ。
			return fmt.Errorf("%w: %w", ErrPoisonMessage, err)
		}
		if lerr := s.linkReversal(ctx, j); lerr != nil {
			return lerr
		}
		_, replayed, err := s.postJournalTx(ctx, j)
		if err != nil {
			return err
		}
		if replayed {
			log.InfoContext(ctx, "journal already posted for event; acked", "journal_id", j.PublicID)
		}
		return nil
	})
}

// linkReversal 為 J-REF-FAIL 找出對應的 J-REF-PEND 並以 reversal_of 關聯（docs/02 §7.3）。
// 找不到（例如 refund.created 事件尚未送達 / 遺失）時不阻擋過帳，僅記 log。
func (s *Service) linkReversal(ctx context.Context, j *domain.Journal) error {
	if j.Template != domain.TemplateJREFFail {
		return nil
	}
	pend, _, err := s.journals.List(ctx, JournalFilter{
		ReferenceType: domain.RefRefund,
		ReferenceID:   j.ReferenceID,
		Template:      domain.TemplateJREFPEND,
	}, Page{Limit: 1})
	if err != nil {
		return err
	}
	if len(pend) == 0 {
		s.log.WarnContext(ctx, "refund.failed without prior refund.created journal; posting unlinked J-REF-FAIL", "refund_id", j.ReferenceID)
		return nil
	}
	orig := pend[0]
	if orig.ReversedBy != nil {
		s.log.WarnContext(ctx, "J-REF-PEND already reversed; posting unlinked J-REF-FAIL", "refund_id", j.ReferenceID, "journal_id", orig.PublicID)
		return nil
	}
	if err := domain.ValidateReversal(orig, &domain.Journal{ReversalOf: &orig.ID, Entries: j.Entries}); err != nil {
		// 金額不同（例如部分失敗）時不強制關聯。
		s.log.WarnContext(ctx, "J-REF-FAIL does not mirror J-REF-PEND; posting unlinked", "refund_id", j.ReferenceID, "err", err)
		return nil
	}
	id := orig.ID
	j.ReversalOf = &id
	return nil
}

// EventIDFromRecord 取出 Kafka record 的 event_id（header 優先，其次 payload）；供 adapter log 使用。
func EventIDFromRecord(rec eventbus.Record) (uuid.UUID, string) {
	if s := rec.EventID(); s != "" {
		if u, err := ParseEventID(s); err == nil {
			return u, s
		}
	}
	return uuid.Nil, rec.EventID()
}

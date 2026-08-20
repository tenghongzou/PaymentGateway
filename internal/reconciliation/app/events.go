package app

import (
	"time"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
)

// 發佈到 reconciliation.events 的事件型別（outbox.event_type）。
//
// Phase 0 以 JSON payload 發佈；TODO(phase-1)：改為 protobuf pg.reconciliation.v1.ReconciliationEvent（見 0002_outbox 註解）。
const (
	EventRunCompleted        = "reconciliation.run.completed"
	EventSettlementPosted    = "settlement.posted"
	EventDiscrepancyResolved = "reconciliation.discrepancy.resolved"

	AggregateRun         = "reconciliation_run"
	AggregateSettlement  = "settlement"
	AggregateDiscrepancy = "discrepancy"

	// HeaderContentType 標示 payload 編碼。
	HeaderContentType = "content-type"
	ContentTypeJSON   = "application/json"
)

// MoneyJSON 為事件內的金額表示。
type MoneyJSON struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// RunCompletedEvent 為 reconciliation.run.completed 的 payload。
type RunCompletedEvent struct {
	EventType   string            `json:"event_type"`
	RunID       string            `json:"run_id"`
	Provider    string            `json:"provider"`
	FileID      string            `json:"file_id"`
	FileHash    string            `json:"file_hash"`
	PeriodStart time.Time         `json:"period_start"`
	PeriodEnd   time.Time         `json:"period_end"`
	Status      string            `json:"status"`
	Summary     domain.RunSummary `json:"summary"`
	OccurredAt  time.Time         `json:"occurred_at"`
}

// SettlementPostedEvent 為 settlement.posted 的 payload（每筆對上的付款一則；ledger 據此記 J-STL，docs/02 §7.3）。
type SettlementPostedEvent struct {
	EventType         string    `json:"event_type"`
	SettlementID      string    `json:"settlement_id"` // = run_id，ledger 以此關聯同批結算
	RunID             string    `json:"run_id"`
	FileID            string    `json:"file_id"`
	Provider          string    `json:"provider"`
	MerchantID        string    `json:"merchant_id"`
	PaymentID         string    `json:"payment_id"` // pay_…
	RecordKind        string    `json:"record_kind"`
	ProviderReference string    `json:"provider_reference"`
	Gross             MoneyJSON `json:"gross"`
	PSPFee            MoneyJSON `json:"psp_fee"`
	NetPaid           MoneyJSON `json:"net_paid"`
	SettledAt         time.Time `json:"settled_at"`
	OccurredAt        time.Time `json:"occurred_at"`
}

// DiscrepancyResolvedEvent 為 reconciliation.discrepancy.resolved 的 payload。
type DiscrepancyResolvedEvent struct {
	EventType         string    `json:"event_type"`
	DiscrepancyID     string    `json:"discrepancy_id"`
	RunID             string    `json:"run_id"`
	Provider          string    `json:"provider"`
	Kind              string    `json:"kind"`
	Status            string    `json:"status"`
	ProviderReference string    `json:"provider_reference,omitempty"`
	InternalReference string    `json:"internal_reference,omitempty"`
	ResolutionNote    string    `json:"resolution_note,omitempty"`
	ResolvedBy        string    `json:"resolved_by"`
	OccurredAt        time.Time `json:"occurred_at"`
}

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// Deps 為 Service 的依賴（ports）。
type Deps struct {
	Tx       TxRunner
	Accounts AccountRepo
	Journals JournalRepo
	Balances BalanceRepo
	Inbox    Inbox
	Outbox   OutboxStore
	Clock    Clock
	Logger   *slog.Logger
	Policy   domain.Policy
}

// Service 實作 ledger 的 use cases。
type Service struct {
	tx       TxRunner
	accounts AccountRepo
	journals JournalRepo
	balances BalanceRepo
	inbox    Inbox
	outbox   OutboxStore
	clock    Clock
	log      *slog.Logger
	policy   domain.Policy
}

// NewService 建立 Service。
func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{
		tx: d.Tx, accounts: d.Accounts, journals: d.Journals, balances: d.Balances,
		inbox: d.Inbox, outbox: d.Outbox, clock: d.Clock, log: d.Logger.With("component", "ledger-app"), policy: d.Policy,
	}
}

// ---------------------------------------------------------------------------
// PostJournal
// ---------------------------------------------------------------------------

// PostJournal 在一個交易內過帳：確保帳戶存在（lazy create）→ 插入 journal + entries → 寫 outbox ledger.journal.posted。
// 以 event_id 唯一做冪等：同一 event_id 重複過帳回傳既有 journal 與 replayed=true，不報錯。
func (s *Service) PostJournal(ctx context.Context, j *domain.Journal) (*domain.Journal, bool, error) {
	if j == nil {
		return nil, false, domain.ErrJournalTooFewEntries
	}
	if err := j.Validate(); err != nil {
		return nil, false, err
	}
	var (
		result   *domain.Journal
		replayed bool
	)
	err := s.tx.RunInTx(ctx, func(ctx context.Context) error {
		r, rep, err := s.postJournalTx(ctx, j)
		result, replayed = r, rep
		return err
	})
	if errors.Is(err, domain.ErrDuplicateEvent) {
		// 與另一個寫入者同時過帳同一 event_id：交易已 rollback，改讀既有 journal（冪等）。
		existing, gerr := s.journals.GetByEventID(ctx, j.EventID)
		if gerr != nil {
			return nil, false, fmt.Errorf("ledger: duplicate event_id %s but journal not readable: %w", j.EventID, err)
		}
		return existing, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}

// postJournalTx 為 PostJournal 的交易內部分（HandlePaymentEvent 與 PostJournal 共用同一個交易）。
func (s *Service) postJournalTx(ctx context.Context, j *domain.Journal) (*domain.Journal, bool, error) {
	// 1. 冪等：event_id 已存在 → 直接回傳。
	existing, err := s.journals.GetByEventID(ctx, j.EventID)
	switch {
	case err == nil:
		return existing, true, nil
	case !errors.Is(err, domain.ErrJournalNotFound):
		return nil, false, err
	}

	// 2. 沖銷：原 journal 必須存在、尚未被沖銷、分錄互為鏡像。
	if j.ReversalOf != nil {
		orig, gerr := s.journals.GetByID(ctx, *j.ReversalOf)
		if gerr != nil {
			return nil, false, gerr
		}
		if verr := domain.ValidateReversal(orig, j); verr != nil {
			return nil, false, verr
		}
	}

	// 3. 帳戶 lazy create 並解析 AccountID；帳戶必須 active。
	resolved := make(map[domain.AccountKey]*domain.Account, len(j.Entries))
	for _, key := range j.AccountKeys() {
		acct, created, aerr := s.accounts.EnsureAccount(ctx, key)
		if aerr != nil {
			return nil, false, aerr
		}
		if created {
			s.log.DebugContext(ctx, "ledger account created", "account", key.String(), "account_id", acct.ID)
		}
		if !acct.CanPost() {
			return nil, false, domain.ErrAccountInactive.WithMessage("account %s is %s", key, acct.Status)
		}
		resolved[key] = acct
	}
	for i := range j.Entries {
		j.Entries[i].AccountID = resolved[j.Entries[i].Account].ID
		if j.Entries[i].ID == uuid.Nil {
			j.Entries[i].ID = ids.NewUUID()
		}
	}

	// 4. ID / 時間 / metadata。
	now := s.clock.Now()
	if j.ID == uuid.Nil {
		j.ID = ids.NewUUID()
	}
	j.PublicID = ids.Format(ids.PrefixJournal, j.ID)
	j.PostedAt = now
	if j.EffectiveAt.IsZero() {
		j.EffectiveAt = now
	}
	if j.Metadata == nil {
		j.Metadata = map[string]string{}
	}
	if j.Template != "" {
		j.Metadata[domain.MetaTemplate] = j.Template
	}
	if j.SourceType != "" {
		j.Metadata[domain.MetaSourceType] = string(j.SourceType)
	}
	if j.SourceID != "" {
		j.Metadata[domain.MetaSourceID] = j.SourceID
	}
	j.Metadata[domain.MetaLivemode] = fmt.Sprintf("%t", j.Livemode)

	// 5. 寫入 journal + entries（同交易；deferred trigger 於 commit 再驗平衡）。
	if ierr := s.journals.Insert(ctx, j); ierr != nil {
		return nil, false, ierr
	}

	// 6. outbox：ledger.journal.posted → ledger.events（同交易，docs/02 §7.6 第 7 點）。
	payload, err := proto.Marshal(ToProtoJournal(j))
	if err != nil {
		return nil, false, fmt.Errorf("ledger: marshal journal event: %w", err)
	}
	aggregateID := MerchantPublicID(j.MerchantID)
	if aggregateID == "" {
		aggregateID = "platform"
	}
	if _, err := s.outbox.Insert(ctx, OutboxMessage{
		AggregateType: AggregateJournal,
		AggregateID:   aggregateID,
		EventType:     EventJournalPosted,
		Payload:       payload,
		Headers: map[string]string{
			"merchant_id":    MerchantPublicID(j.MerchantID),
			"journal_id":     j.PublicID,
			"reference_type": string(j.ReferenceType),
			"reference_id":   j.ReferenceID,
			"template":       j.Template,
			"livemode":       fmt.Sprintf("%t", j.Livemode),
			"content-type":   "application/x-protobuf",
		},
	}); err != nil {
		return nil, false, err
	}
	s.log.InfoContext(ctx, "journal posted",
		"journal_id", j.PublicID, "template", j.Template, "reference_type", j.ReferenceType, "reference_id", j.ReferenceID,
		"currency", j.Currency(), "total", j.TotalDebit(), "entries", len(j.Entries))
	return j, false, nil
}

// ---------------------------------------------------------------------------
// 手動過帳（gRPC PostJournal）
// ---------------------------------------------------------------------------

// EntryInput 為手動過帳的分錄輸入（以 account_id 指定帳戶）。
type EntryInput struct {
	AccountID   uuid.UUID
	Direction   domain.Direction
	Amount      money.Money
	Description string
}

// PostJournalInput 為手動過帳輸入（對應 PostJournalRequest）。
type PostJournalInput struct {
	IdempotencyKey    string
	MerchantID        uuid.UUID // Nil = 系統 journal
	SourceType        domain.SourceType
	SourceID          string
	ReferenceType     domain.ReferenceType // 空則依 SourceType 推導
	ReferenceID       string
	Description       string
	Livemode          bool
	Metadata          map[string]string
	EffectiveAt       time.Time
	ReversesJournalID *uuid.UUID
	Entries           []EntryInput
}

// PostManualJournal 供內部呼叫（reconciliation 調整、後台沖銷）：依 account_id 解析帳戶後建立 journal 並過帳。
// source_type 不可為 payment_event（該類型只由 consumer 寫入）。
func (s *Service) PostManualJournal(ctx context.Context, in PostJournalInput) (*domain.Journal, bool, error) {
	eventID, err := IdempotencyKeyToEventID(in.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if in.SourceType == domain.SourcePaymentEvent {
		return nil, false, apperr.ErrParameterInvalid.WithParam("source_type").WithMessage("source_type PAYMENT_EVENT is reserved for the payment.events consumer")
	}
	if in.Description == "" {
		return nil, false, apperr.ErrParameterMissing.WithParam("description").WithMessage("description is required")
	}
	if len(in.Entries) < 2 {
		return nil, false, domain.ErrJournalTooFewEntries
	}
	refType := in.ReferenceType
	if refType == "" {
		switch in.SourceType {
		case domain.SourceReversal:
			refType = domain.RefReversal
		case domain.SourcePayout:
			refType = domain.RefSettlement
		case domain.SourceReconciliationAdjustment, domain.SourceManualAdjustment, domain.SourcePaymentEvent:
			// payment_event 已在上方被拒絕，列出僅為 switch 完整性。
			refType = domain.RefAdjustment
		default:
			refType = domain.RefAdjustment
		}
	}
	if in.ReversesJournalID != nil && in.SourceType != domain.SourceReversal {
		return nil, false, apperr.ErrParameterInvalid.WithParam("source_type").WithMessage("reverses_journal_id requires source_type REVERSAL")
	}
	j := &domain.Journal{
		EventID:       eventID,
		MerchantID:    in.MerchantID,
		Livemode:      in.Livemode,
		SourceType:    in.SourceType,
		SourceID:      in.SourceID,
		ReferenceType: refType,
		ReferenceID:   in.ReferenceID,
		Description:   in.Description,
		EffectiveAt:   in.EffectiveAt,
		ReversalOf:    in.ReversesJournalID,
		Metadata:      map[string]string{},
	}
	if in.SourceType == domain.SourceReversal {
		j.Template = domain.TemplateJREV
	}
	for k, v := range in.Metadata {
		j.Metadata[k] = v
	}
	if j.ReferenceID == "" {
		j.ReferenceID = in.SourceID
	}
	if j.ReferenceID == "" {
		j.ReferenceID = in.IdempotencyKey
	}
	// 解析帳戶（交易外讀取即可；過帳交易內 EnsureAccount 會再次取得同一列）。
	for i, e := range in.Entries {
		acct, err := s.accounts.GetByID(ctx, e.AccountID)
		if err != nil {
			return nil, false, err
		}
		if !acct.CanPost() {
			return nil, false, domain.ErrAccountInactive.WithMessage("entries[%d]: account %s is %s", i, AccountPublicID(acct.ID), acct.Status)
		}
		j.Entries = append(j.Entries, domain.Entry{
			AccountID:   acct.ID,
			Account:     acct.Key,
			Direction:   e.Direction,
			Amount:      e.Amount,
			Description: e.Description,
		})
	}
	return s.PostJournal(ctx, j)
}

// ReverseJournal 以 J-REV 沖銷既有 journal（ops 工具）。idempotencyKey 為新 journal 的冪等鍵。
func (s *Service) ReverseJournal(ctx context.Context, journalID uuid.UUID, idempotencyKey, description string) (*domain.Journal, bool, error) {
	eventID, err := IdempotencyKeyToEventID(idempotencyKey)
	if err != nil {
		return nil, false, err
	}
	// 冪等：同一 idempotency key 重放時直接回傳既有沖銷 journal（此時原 journal 已帶 ReversedBy，domain.Reverse 會拒絕）。
	if existing, gerr := s.journals.GetByEventID(ctx, eventID); gerr == nil {
		return existing, true, nil
	} else if !errors.Is(gerr, domain.ErrJournalNotFound) {
		return nil, false, gerr
	}
	orig, err := s.journals.GetByID(ctx, journalID)
	if err != nil {
		return nil, false, err
	}
	rev, err := domain.Reverse(orig, eventID, description, s.clock.Now())
	if err != nil {
		return nil, false, err
	}
	return s.PostJournal(ctx, rev)
}

// ---------------------------------------------------------------------------
// 查詢
// ---------------------------------------------------------------------------

// GetJournal 取得 journal；merchantID 非 Nil 時限制只能看到該商戶的 journal（其餘視為不存在）。
func (s *Service) GetJournal(ctx context.Context, id uuid.UUID, merchantID uuid.UUID) (*domain.Journal, error) {
	j, err := s.journals.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if merchantID != uuid.Nil && j.MerchantID != merchantID {
		return nil, domain.ErrJournalNotFound
	}
	return j, nil
}

// ListJournals 依條件列出 journal。
func (s *Service) ListJournals(ctx context.Context, f JournalFilter, page Page) ([]*domain.Journal, *Cursor, error) {
	if page.Limit <= 0 {
		page.Limit = DefaultPageSize
	}
	return s.journals.List(ctx, f, page)
}

// GetAccount 取得帳戶。
func (s *Service) GetAccount(ctx context.Context, id uuid.UUID) (*domain.Account, error) {
	return s.accounts.GetByID(ctx, id)
}

// ListAccounts 依條件列出帳戶。
func (s *Service) ListAccounts(ctx context.Context, f AccountFilter, page Page) ([]*domain.Account, *Cursor, error) {
	if page.Limit <= 0 {
		page.Limit = DefaultPageSize
	}
	return s.accounts.List(ctx, f, page)
}

// CreateAccount 建立帳戶（冪等：已存在回傳既有帳戶與 alreadyExisted=true）。
func (s *Service) CreateAccount(ctx context.Context, key domain.AccountKey) (*domain.Account, bool, error) {
	if err := key.Validate(); err != nil {
		return nil, false, err
	}
	var (
		acct    *domain.Account
		created bool
	)
	err := s.tx.RunInTx(ctx, func(ctx context.Context) error {
		a, c, err := s.accounts.EnsureAccount(ctx, key)
		acct, created = a, c
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return acct, !created, nil
}

// GetBalance 取得單一帳戶餘額。
func (s *Service) GetBalance(ctx context.Context, accountID uuid.UUID) (*domain.Balance, error) {
	return s.balances.GetByAccount(ctx, accountID)
}

// GetMerchantBalances 取得商戶的餘額拆解（每幣別一筆；currency 空 = 全部）。
func (s *Service) GetMerchantBalances(ctx context.Context, merchantID uuid.UUID, currency string, livemode bool) ([]domain.MerchantBalance, error) {
	if merchantID == uuid.Nil {
		return nil, apperr.ErrParameterMissing.WithParam("merchant_id").WithMessage("merchant_id is required")
	}
	if currency != "" && !money.IsSupportedCurrency(currency) {
		return nil, domain.ErrInvalidCurrency
	}
	rows, err := s.balances.ListByMerchant(ctx, merchantID, currency, livemode)
	if err != nil {
		return nil, err
	}
	byCurrency := map[string]*domain.MerchantBalance{}
	var order []string
	for _, b := range rows {
		mb, ok := byCurrency[b.Account.Currency]
		if !ok {
			mb = &domain.MerchantBalance{MerchantID: merchantID, Currency: b.Account.Currency, Livemode: livemode}
			byCurrency[b.Account.Currency] = mb
			order = append(order, b.Account.Currency)
		}
		switch b.Account.Kind() {
		case domain.KindMerchantPayable:
			mb.Payable = b.Balance
		case domain.KindRefundClearing:
			mb.Pending = b.Balance
		case domain.KindChargebackReserve:
			mb.Reserved = b.Balance
		case domain.KindPSPReceivable, domain.KindBankCash, domain.KindSettlementSuspense,
			domain.KindFeeRevenue, domain.KindChargebackFeeRevenue,
			domain.KindPSPFeeExpense, domain.KindChargebackFeeExpense:
			// 系統科目不屬於商戶餘額拆解（ListByMerchant 依 merchant_id 過濾，正常不會出現）。
		}
		if b.UpdatedAt.After(mb.AsOf) {
			mb.AsOf = b.UpdatedAt
		}
	}
	out := make([]domain.MerchantBalance, 0, len(order))
	for _, c := range order {
		mb := byCurrency[c]
		// 範本已把退款預扣 / 爭議凍結自 merchant_payable 移出，因此可用餘額 = merchant_payable（見 domain.MerchantBalance 註解）。
		mb.Available = mb.Payable
		if mb.AsOf.IsZero() {
			mb.AsOf = s.clock.Now()
		}
		out = append(out, *mb)
	}
	if len(out) == 0 && currency != "" {
		// 商戶尚無任何帳戶：回傳零餘額，讓 API 行為穩定。
		out = append(out, domain.MerchantBalance{MerchantID: merchantID, Currency: currency, Livemode: livemode, AsOf: s.clock.Now()})
	}
	return out, nil
}

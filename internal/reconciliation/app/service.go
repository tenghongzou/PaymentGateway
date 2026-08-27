package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// 分頁預設值（docs/03 PageRequest：0 → 20，上限 100）。
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Config 為 use case 的可調參數。
type Config struct {
	// GracePeriod 為 missing_in_psp 的寬限期：內部紀錄 occurred_at 距今未超過此值不開單（PSP 延遲結算）。預設 72h。
	GracePeriod time.Duration
	// ConsumerName 為 processed_events.consumer。預設 recon.payment-projector。
	ConsumerName string
	// MaxUnsettled 為每次 run 載入「應結算但未對上」紀錄的上限。預設 10000。
	MaxUnsettled int
}

func (c *Config) defaults() {
	if c.GracePeriod == 0 {
		c.GracePeriod = 72 * time.Hour
	}
	if c.ConsumerName == "" {
		c.ConsumerName = "recon.payment-projector"
	}
	if c.MaxUnsettled <= 0 {
		c.MaxUnsettled = 10000
	}
}

// Deps 為 Service 的相依（全部為 port）。
type Deps struct {
	Tx            TxManager
	Files         FileRepo
	Lines         LineRepo
	Records       PaymentRecordRepo
	Runs          RunRepo
	Discrepancies DiscrepancyRepo
	Inbox         Inbox
	Outbox        OutboxStore
	Clock         Clock
	Logger        *slog.Logger
	// ParserFor 可替換 parser 查找（預設 domain.ParserFor）。
	ParserFor func(domain.Format) (domain.Parser, error)
}

// Service 實作所有 use cases。
type Service struct {
	d       Deps
	cfg     Config
	matcher *domain.Matcher
}

// NewService 建立 Service。
func NewService(d Deps, cfg Config) *Service {
	cfg.defaults()
	if d.Clock == nil {
		d.Clock = SystemClock()
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.ParserFor == nil {
		d.ParserFor = domain.ParserFor
	}
	return &Service{d: d, cfg: cfg, matcher: domain.NewMatcher()}
}

// Config 回傳生效的設定。
func (s *Service) Config() Config { return s.cfg }

// ---------------------------------------------------------------------------
// ImportSettlementFile
// ---------------------------------------------------------------------------

// ImportCommand 為匯入結算檔的輸入。
type ImportCommand struct {
	Provider string
	Format   domain.Format
	FileName string
	Content  []byte
	// SettlementDate 為結算日 YYYY-MM-DD（run 的 period）。
	SettlementDate string
	TriggeredBy    string
	// StorageURI 為原檔位置（可空）。
	StorageURI string
	// ExpectedChecksum 為呼叫端提供的 sha256 hex（可空；提供時驗證）。
	ExpectedChecksum string
}

// ImportResult 為匯入結果。
type ImportResult struct {
	Run  *domain.Run
	File *domain.SettlementFile
	// AlreadyImported 為 true 表示同一檔案先前已匯入並完成比對，回傳既有 run。
	AlreadyImported bool
}

// ImportSettlementFile 匯入結算檔並同步執行一次比對：
//
//	file_hash 查重 → 解析 → 寫 settlement_lines → 建 run → 比對本地讀模型 → 寫 discrepancies
//	→ outbox（reconciliation.run.completed、每筆對上的付款一則 settlement.posted）。
//
// 全部業務寫入在同一交易；解析失敗時另以獨立交易把檔案標為 failed（docs/05 §9.3）。
func (s *Service) ImportSettlementFile(ctx context.Context, cmd ImportCommand) (*ImportResult, error) {
	if strings.TrimSpace(cmd.Provider) == "" {
		return nil, apperr.ErrParameterMissing.WithMessage("provider is required.").WithParam("provider")
	}
	if strings.TrimSpace(cmd.FileName) == "" {
		return nil, apperr.ErrParameterMissing.WithMessage("file_name is required.").WithParam("file_name")
	}
	parser, err := s.d.ParserFor(cmd.Format)
	if err != nil {
		return nil, err
	}
	if parser.Provider() != cmd.Provider {
		return nil, domain.ErrUnknownFormat.WithMessage("Format %q is not valid for provider %q.", string(cmd.Format), cmd.Provider)
	}
	periodStart, periodEnd, err := domain.PeriodForDate(cmd.SettlementDate)
	if err != nil {
		return nil, err
	}
	hash := domain.FileHash(cmd.Content)
	if cmd.ExpectedChecksum != "" && !strings.EqualFold(cmd.ExpectedChecksum, hash) {
		return nil, apperr.ErrParameterInvalid.WithMessage("expected_checksum does not match file content (got %s).", hash).WithParam("expected_checksum")
	}

	// 1. 快速路徑：同檔已匯入且有 run → 直接回傳。
	imported, err := s.findImported(ctx, hash)
	if err != nil || imported != nil {
		return imported, err
	}

	// 2. 解析（在交易外，避免長時間持有交易）。
	lines, parseErr := parser.Parse(bytes.NewReader(cmd.Content))
	if parseErr != nil {
		if ferr := s.recordParseFailure(ctx, cmd, hash, periodStart, periodEnd, parseErr); ferr != nil {
			s.d.Logger.ErrorContext(ctx, "record settlement file failure", "err", ferr, "file_hash", hash)
		}
		return nil, parseErr
	}

	// 3. 主交易。
	var out *ImportResult
	txErr := s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.d.Clock.Now()
		file, err := s.loadOrCreateFile(ctx, cmd, hash, periodStart, periodEnd, now)
		if err != nil {
			return err
		}
		if file.Status == domain.FileImported {
			// 曾匯入但沒有 run（匯入到一半崩潰）：以 DB 內既有的 lines 重做比對。
			var prev *domain.Run
			if prev, err = s.d.Runs.FindByFileID(ctx, file.ID); err != nil {
				return err
			}
			if prev != nil {
				out = &ImportResult{Run: prev, File: file, AlreadyImported: true}
				return nil
			}
			var existing []domain.SettlementLine
			if existing, err = s.d.Lines.ListByFile(ctx, file.ID); err != nil {
				return err
			}
			if len(existing) > 0 {
				lines = existing
			}
		}
		for i := range lines {
			if lines[i].ID == uuid.Nil {
				lines[i].ID = ids.NewUUID()
			}
			lines[i].FileID = file.ID
			lines[i].CreatedAt = now
		}
		if insErr := s.d.Lines.InsertBatch(ctx, lines); insErr != nil {
			return insErr
		}
		file.MarkImported(len(lines), now)
		if upErr := s.d.Files.Update(ctx, file); upErr != nil {
			return upErr
		}

		run, err := domain.NewRun(cmd.Provider, periodStart, periodEnd, cmd.TriggeredBy, now)
		if err != nil {
			return err
		}
		run.Summary.FileID = file.ID.String()
		run.Summary.FileName = file.FileName
		run.Summary.FileHash = file.FileHash
		run.Summary.FileFormat = string(cmd.Format)
		run.Summary.FileSizeBytes = int64(len(cmd.Content))
		if err := s.d.Runs.Create(ctx, run); err != nil {
			return err
		}
		if err := run.Start(now); err != nil {
			return err
		}
		if err := s.reconcile(ctx, run, file, lines); err != nil {
			return err
		}
		out = &ImportResult{Run: run, File: file}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return out, nil
}

// findImported 回傳同 hash 且已完成比對的 run；沒有時回 (nil, nil)。
func (s *Service) findImported(ctx context.Context, hash string) (*ImportResult, error) {
	var out *ImportResult
	err := s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		file, err := s.d.Files.GetByHash(ctx, hash)
		if errors.Is(err, domain.ErrFileNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if file.Status != domain.FileImported {
			return nil
		}
		run, err := s.d.Runs.FindByFileID(ctx, file.ID)
		if err != nil || run == nil {
			return err
		}
		out = &ImportResult{Run: run, File: file, AlreadyImported: true}
		return nil
	})
	return out, err
}

// loadOrCreateFile 取得或建立 settlement_files 列；既有 failed 檔案重新標為 importing。
func (s *Service) loadOrCreateFile(ctx context.Context, cmd ImportCommand, hash string, start, end time.Time, now time.Time) (*domain.SettlementFile, error) {
	file, err := s.d.Files.GetByHash(ctx, hash)
	switch {
	case errors.Is(err, domain.ErrFileNotFound):
		file = domain.NewSettlementFile(cmd.Provider, cmd.FileName, hash, &start, &end, now)
		file.StorageURI = cmd.StorageURI
		file.Metadata["format"] = string(cmd.Format)
		if cmd.TriggeredBy != "" {
			file.Metadata["triggered_by"] = cmd.TriggeredBy
		}
		if err = s.d.Files.Create(ctx, file); err != nil {
			if !errors.Is(err, domain.ErrDuplicateFile) {
				return nil, err
			}
			// 並發匯入同一檔案：改讀既有列。
			if file, err = s.d.Files.GetByHash(ctx, hash); err != nil {
				return nil, err
			}
		}
	case err != nil:
		return nil, err
	}
	if file.Status != domain.FileImported {
		file.MarkImporting(now)
		if err := s.d.Files.Update(ctx, file); err != nil {
			return nil, err
		}
	}
	return file, nil
}

// recordParseFailure 以獨立交易把檔案標為 failed（保留 hash，讓修正 parser 後可重新匯入）。
func (s *Service) recordParseFailure(ctx context.Context, cmd ImportCommand, hash string, start, end time.Time, cause error) error {
	return s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.d.Clock.Now()
		file, err := s.d.Files.GetByHash(ctx, hash)
		if errors.Is(err, domain.ErrFileNotFound) {
			file = domain.NewSettlementFile(cmd.Provider, cmd.FileName, hash, &start, &end, now)
			file.StorageURI = cmd.StorageURI
			file.Metadata["format"] = string(cmd.Format)
			file.MarkFailed(truncate(cause.Error(), 1000), now)
			if err = s.d.Files.Create(ctx, file); err != nil && !errors.Is(err, domain.ErrDuplicateFile) {
				return err
			}
			return nil
		}
		if err != nil {
			return err
		}
		if file.Status == domain.FileImported {
			return nil
		}
		file.MarkFailed(truncate(cause.Error(), 1000), now)
		return s.d.Files.Update(ctx, file)
	})
}

// reconcile 對 run 執行比對並寫入差異與 outbox（呼叫端已在交易內）。
func (s *Service) reconcile(ctx context.Context, run *domain.Run, file *domain.SettlementFile, lines []domain.SettlementLine) error {
	now := s.d.Clock.Now()
	refs := make([]string, 0, len(lines))
	seen := map[string]bool{}
	for _, l := range lines {
		if l.Type.Matchable() && !seen[l.ProviderReference] {
			seen[l.ProviderReference] = true
			refs = append(refs, l.ProviderReference)
		}
	}
	records, err := s.d.Records.FindByProviderRefs(ctx, run.Provider, refs)
	if err != nil {
		return err
	}
	unsettled, err := s.d.Records.ListUnsettled(ctx, run.Provider, now.Add(-s.cfg.GracePeriod), s.cfg.MaxUnsettled)
	if err != nil {
		return err
	}
	res := s.matcher.Match(domain.MatchInput{
		Provider:    run.Provider,
		Lines:       lines,
		Records:     append(records, unsettled...),
		Now:         now,
		GracePeriod: s.cfg.GracePeriod,
	})

	// 跨 run 去重：同 provider / kind / 參照已有 open 差異則不重複開單。
	toInsert := make([]domain.Discrepancy, 0, len(res.Discrepancies))
	suppressed := 0
	for i := range res.Discrepancies {
		d := &res.Discrepancies[i]
		exists, err := s.d.Discrepancies.ExistsOpen(ctx, run.Provider, d.Kind, d.ProviderReference, d.InternalReference)
		if err != nil {
			return err
		}
		if exists {
			suppressed++
			continue
		}
		d.RunID = run.ID
		d.CreatedAt = now
		d.UpdatedAt = now
		toInsert = append(toInsert, *d)
	}
	if err := s.d.Discrepancies.InsertBatch(ctx, toInsert); err != nil {
		return err
	}
	if err := run.Complete(res, now); err != nil {
		return err
	}
	run.Summary.Suppressed = suppressed
	if err := s.d.Runs.Update(ctx, run); err != nil {
		return err
	}

	// outbox：run.completed + 每筆對上的付款一則 settlement.posted。
	if err := s.emit(ctx, AggregateRun, run.PublicID, EventRunCompleted, RunCompletedEvent{
		EventType: EventRunCompleted, RunID: run.PublicID, Provider: run.Provider,
		FileID: file.ID.String(), FileHash: file.FileHash,
		PeriodStart: run.PeriodStart, PeriodEnd: run.PeriodEnd, Status: string(run.Status),
		Summary: run.Summary, OccurredAt: now,
	}); err != nil {
		return err
	}
	for i := range res.Matched {
		p := &res.Matched[i]
		if p.Record.Kind != domain.RecordPayment {
			continue
		}
		ev := SettlementPostedEvent{
			EventType: EventSettlementPosted, SettlementID: run.PublicID, RunID: run.PublicID, FileID: file.ID.String(),
			Provider: run.Provider, MerchantID: p.Record.MerchantID.String(), PaymentID: p.Record.PublicID,
			RecordKind: string(p.Record.Kind), ProviderReference: p.Line.ProviderReference,
			Gross:     MoneyJSON{AmountMinor: p.Line.Amount.AmountMinor, Currency: p.Line.Amount.Currency},
			PSPFee:    MoneyJSON{AmountMinor: p.Line.Fee.AmountMinor, Currency: p.Line.Amount.Currency},
			NetPaid:   MoneyJSON{AmountMinor: p.Line.Net().AmountMinor, Currency: p.Line.Amount.Currency},
			SettledAt: p.Line.SettledAt, OccurredAt: now,
		}
		if err := s.emit(ctx, AggregateSettlement, p.Record.PublicID, EventSettlementPosted, ev); err != nil {
			return err
		}
	}
	s.d.Logger.InfoContext(ctx, "reconciliation run completed",
		"run_id", run.PublicID, "provider", run.Provider, "lines", res.TotalLines,
		"matched", len(res.Matched), "discrepancies", len(res.Discrepancies), "suppressed", suppressed, "deferred", res.Deferred)
	return nil
}

// emit 把事件以 JSON 寫入 outbox。
func (s *Service) emit(ctx context.Context, aggregateType, aggregateID, eventType string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", eventType, err)
	}
	_, err = s.d.Outbox.Insert(ctx, OutboxMessage{
		AggregateType: aggregateType, AggregateID: aggregateID, EventType: eventType, Payload: b,
		Headers: map[string]string{HeaderContentType: ContentTypeJSON},
	})
	return err
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// GetReconciliationRun 以對外 ID（rr_…）取得 run。
func (s *Service) GetReconciliationRun(ctx context.Context, publicID string) (*domain.Run, error) {
	id, err := domain.ParseRunID(publicID)
	if err != nil {
		return nil, err
	}
	return s.d.Runs.GetByID(ctx, id)
}

// ListReconciliationRuns 列出 run。
func (s *Service) ListReconciliationRuns(ctx context.Context, f RunFilter) ([]domain.Run, string, error) {
	f.PageSize = clampPageSize(f.PageSize)
	return s.d.Runs.List(ctx, f)
}

// ListDiscrepancies 列出差異。
func (s *Service) ListDiscrepancies(ctx context.Context, f DiscrepancyFilter) ([]domain.Discrepancy, string, error) {
	f.PageSize = clampPageSize(f.PageSize)
	for _, k := range f.Kinds {
		if !k.IsValid() {
			return nil, "", apperr.ErrParameterInvalid.WithMessage("unknown discrepancy kind %q.", string(k)).WithParam("types")
		}
	}
	return s.d.Discrepancies.List(ctx, f)
}

// ---------------------------------------------------------------------------
// ResolveDiscrepancy
// ---------------------------------------------------------------------------

// ResolutionAction 為處理差異的動作。
type ResolutionAction string

// 支援的動作（ADJUST_LEDGER / RESYNC_PAYMENT 為 Phase 1+，由 adapter 回 Unimplemented）。
const (
	ActionResolve ResolutionAction = "resolve"
	ActionIgnore  ResolutionAction = "ignore"
)

// ResolveCommand 為處理差異的輸入。
type ResolveCommand struct {
	DiscrepancyID  string // dsc_… 或 uuid
	Action         ResolutionAction
	Note           string
	ResolvedBy     string
	IdempotencyKey string
}

// ResolveDiscrepancy 把 open 差異標為 resolved / ignored，並發出 discrepancy.resolved 事件。
// 冪等：同一 idempotency_key 重送時回傳既有結果。
func (s *Service) ResolveDiscrepancy(ctx context.Context, cmd ResolveCommand) (*domain.Discrepancy, error) {
	id, err := domain.ParseDiscrepancyID(cmd.DiscrepancyID)
	if err != nil {
		return nil, err
	}
	var out *domain.Discrepancy
	txErr := s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		d, err := s.d.Discrepancies.GetByID(ctx, id)
		if err != nil {
			return err
		}
		if cmd.IdempotencyKey != "" && !d.IsOpen() && d.Detail(domain.DetailIdempotencyKey) == cmd.IdempotencyKey {
			out = d
			return nil
		}
		now := s.d.Clock.Now()
		switch cmd.Action {
		case ActionResolve:
			err = d.Resolve(cmd.Note, cmd.ResolvedBy, now)
		case ActionIgnore:
			err = d.Ignore(cmd.Note, cmd.ResolvedBy, now)
		default:
			err = apperr.ErrParameterInvalid.WithMessage("unsupported resolution action %q.", string(cmd.Action)).WithParam("action")
		}
		if err != nil {
			return err
		}
		if cmd.IdempotencyKey != "" {
			if d.Details == nil {
				d.Details = map[string]any{}
			}
			d.Details[domain.DetailIdempotencyKey] = cmd.IdempotencyKey
		}
		if upErr := s.d.Discrepancies.Update(ctx, d); upErr != nil {
			return upErr
		}
		run, err := s.d.Runs.GetByID(ctx, d.RunID)
		runPublicID := ""
		if err == nil {
			runPublicID = run.PublicID
		}
		if err := s.emit(ctx, AggregateDiscrepancy, d.PublicID(), EventDiscrepancyResolved, DiscrepancyResolvedEvent{
			EventType: EventDiscrepancyResolved, DiscrepancyID: d.PublicID(), RunID: runPublicID, Provider: d.Provider,
			Kind: string(d.Kind), Status: string(d.Status), ProviderReference: d.ProviderReference, InternalReference: d.InternalReference,
			ResolutionNote: d.ResolutionNote, ResolvedBy: d.ResolvedBy, OccurredAt: now,
		}); err != nil {
			return err
		}
		out = d
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return out, nil
}

func clampPageSize(n int) int {
	if n <= 0 {
		return DefaultPageSize
	}
	if n > MaxPageSize {
		return MaxPageSize
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

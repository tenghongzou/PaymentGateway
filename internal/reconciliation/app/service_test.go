package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app/porttest"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

var (
	now        = time.Date(2026, 8, 20, 6, 0, 0, 0, time.UTC)
	merchantID = ids.NewUUID()
)

const header = "type,provider_reference,amount_minor,currency,fee_minor,settled_at\n"

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "settlement", "mock-2026-08-19.csv"))
	require.NoError(t, err)
	return b
}

func newSvc(t *testing.T, cfg app.Config) (*app.Service, *porttest.Store, *porttest.Clock) {
	t.Helper()
	store := porttest.NewStore()
	clock := porttest.NewClock(now)
	return app.NewService(store.Deps(clock), cfg), store, clock
}

func rec(kind domain.RecordKind, ref, status string, amount int64, occurred time.Time) domain.PaymentRecord {
	u := ids.NewUUID()
	prefix := map[domain.RecordKind]string{domain.RecordPayment: ids.PrefixPayment, domain.RecordRefund: ids.PrefixRefund, domain.RecordDispute: ids.PrefixDispute}[kind]
	return domain.PaymentRecord{
		ID: u, Kind: kind, PublicID: ids.Format(prefix, u), MerchantID: merchantID, Provider: domain.MockProvider,
		ProviderReference: ref, Amount: money.Money{AmountMinor: amount, Currency: "TWD"}, Status: status, OccurredAt: occurred, SourceSeq: 1,
	}
}

func importCmd(content []byte) app.ImportCommand {
	return app.ImportCommand{
		Provider: domain.MockProvider, Format: domain.FormatMockCSV, FileName: "mock-2026-08-19.csv",
		Content: content, SettlementDate: "2026-08-19", TriggeredBy: "test",
	}
}

func TestImportSettlementFile_Success(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{GracePeriod: 72 * time.Hour})
	old := now.Add(-5 * 24 * time.Hour)
	// 對應 fixture：G3 完全對上、G4 金額不符、G5 缺少（missing_in_ledger）、H1 退款對上、J1 拒付對上；另一筆很舊的已請款付款不在檔案內。
	store.AddRecord(rec(domain.RecordPayment, "mock_ch_01K2W3X4Y5Z6A7B8C9D0E1F2G3", domain.StatusCaptured, 1000, old))
	store.AddRecord(rec(domain.RecordPayment, "mock_ch_01K2W3X4Y5Z6A7B8C9D0E1F2G4", domain.StatusCaptured, 250001, old))
	store.AddRecord(rec(domain.RecordRefund, "mock_re_01K2W3X4Y5Z6A7B8C9D0E1F2H1", domain.RefundSucceeded, 300, old))
	store.AddRecord(rec(domain.RecordDispute, "mock_du_01K2W3X4Y5Z6A7B8C9D0E1F2J1", domain.DisputeLost, 1000, old))
	missing := rec(domain.RecordPayment, "mock_ch_OLD", domain.StatusCaptured, 77, old)
	store.AddRecord(missing)
	// grace 期內的紀錄不開單（repo 的 ListUnsettled 以 now-grace 為上限，根本不會載入）。
	store.AddRecord(rec(domain.RecordPayment, "mock_ch_RECENT", domain.StatusCaptured, 88, now.Add(-time.Hour)))

	res, err := svc.ImportSettlementFile(context.Background(), importCmd(fixture(t)))
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.AlreadyImported)

	run := res.Run
	assert.Equal(t, domain.RunCompleted, run.Status)
	assert.Equal(t, domain.MockProvider, run.Provider)
	assert.Equal(t, "2026-08-19", run.SettlementDate())
	assert.Equal(t, "test", run.TriggeredBy)
	assert.Equal(t, 3, run.MatchedCount, "G3 + H1 + J1")
	assert.Equal(t, 3, run.UnmatchedCount, "G4 amount + G5 missing_in_ledger + OLD missing_in_psp")
	assert.Equal(t, 6, run.Summary.TotalLines)
	assert.Equal(t, 1, run.Summary.Skipped, "fee 列")
	assert.Equal(t, 0, run.Summary.Deferred, "grace 期內紀錄由 repo 過濾，不計入 deferred")
	assert.Equal(t, map[string]int{"amount_mismatch": 1, "missing_in_ledger": 1, "missing_in_psp": 1}, run.Summary.ByKind)
	assert.Equal(t, res.File.ID.String(), run.Summary.FileID)
	assert.Equal(t, int64(len(fixture(t))), run.Summary.FileSizeBytes)
	assert.Equal(t, string(domain.FormatMockCSV), run.Summary.FileFormat)

	file := res.File
	assert.Equal(t, domain.FileImported, file.Status)
	assert.Equal(t, 6, file.RowCount)
	assert.Equal(t, domain.FileHash(fixture(t)), file.FileHash)
	require.NotNil(t, file.PeriodStart)
	assert.Equal(t, "2026-08-19", file.PeriodStart.Format("2006-01-02"))

	assert.Len(t, store.Lines[file.ID], 6)
	for _, l := range store.Lines[file.ID] {
		assert.Equal(t, file.ID, l.FileID)
		assert.NotEqual(t, uuid.Nil, l.ID)
	}

	assert.Len(t, store.Discrepancies, 3)
	for _, d := range store.Discrepancies {
		assert.Equal(t, run.ID, d.RunID)
		assert.Equal(t, domain.DiscrepancyOpen, d.Status)
		if d.Kind == domain.KindMissingInPSP {
			assert.Equal(t, missing.PublicID, d.InternalReference)
		}
	}

	completed := store.OutboxByType(app.EventRunCompleted)
	require.Len(t, completed, 1)
	assert.Equal(t, app.AggregateRun, completed[0].AggregateType)
	assert.Equal(t, run.PublicID, completed[0].AggregateID)
	assert.Equal(t, app.ContentTypeJSON, completed[0].Headers[app.HeaderContentType])
	var ev app.RunCompletedEvent
	require.NoError(t, json.Unmarshal(completed[0].Payload, &ev))
	assert.Equal(t, run.PublicID, ev.RunID)
	assert.Equal(t, 3, ev.Summary.Matched)

	posted := store.OutboxByType(app.EventSettlementPosted)
	require.Len(t, posted, 1, "只有對上的 payment（G3）發 settlement.posted；refund / chargeback 不發")
	var sp app.SettlementPostedEvent
	require.NoError(t, json.Unmarshal(posted[0].Payload, &sp))
	assert.Equal(t, run.PublicID, sp.SettlementID)
	assert.Equal(t, "mock_ch_01K2W3X4Y5Z6A7B8C9D0E1F2G3", sp.ProviderReference)
	assert.Equal(t, int64(1000), sp.Gross.AmountMinor)
	assert.Equal(t, int64(59), sp.PSPFee.AmountMinor)
	assert.Equal(t, int64(941), sp.NetPaid.AmountMinor)
	assert.Equal(t, "TWD", sp.NetPaid.Currency)
	assert.Equal(t, merchantID.String(), sp.MerchantID)
	assert.True(t, ids.HasPrefix(sp.PaymentID, ids.PrefixPayment))
	assert.Equal(t, sp.PaymentID, posted[0].AggregateID)
}

func TestImportSettlementFile_Idempotent(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{})
	first, err := svc.ImportSettlementFile(context.Background(), importCmd(fixture(t)))
	require.NoError(t, err)
	outboxBefore := len(store.Outbox)

	second, err := svc.ImportSettlementFile(context.Background(), importCmd(fixture(t)))
	require.NoError(t, err)
	assert.True(t, second.AlreadyImported)
	assert.Equal(t, first.Run.ID, second.Run.ID)
	assert.Len(t, store.Runs, 1)
	assert.Len(t, store.Files, 1)
	assert.Len(t, store.Outbox, outboxBefore, "重複匯入不再發事件")

	// 不同檔名、同內容仍視為同檔。
	cmd := importCmd(fixture(t))
	cmd.FileName = "renamed.csv"
	third, err := svc.ImportSettlementFile(context.Background(), cmd)
	require.NoError(t, err)
	assert.True(t, third.AlreadyImported)
}

func TestImportSettlementFile_ConcurrentCreateRace(t *testing.T) {
	// 模擬 findImported 查無、Create 時才被別人插入：fake Create 回 ErrDuplicateFile → 改讀既有檔。
	svc, store, _ := newSvc(t, app.Config{})
	content := fixture(t)
	existing := domain.NewSettlementFile(domain.MockProvider, "x.csv", domain.FileHash(content), nil, nil, now)
	existing.Status = domain.FileFailed
	store.Files[existing.ID] = existing
	store.Errs["files.create"] = domain.ErrDuplicateFile

	res, err := svc.ImportSettlementFile(context.Background(), importCmd(content))
	require.NoError(t, err)
	assert.Equal(t, existing.ID, res.File.ID)
	assert.Equal(t, domain.FileImported, res.File.Status)
	assert.Len(t, store.Files, 1)
}

func TestImportSettlementFile_ParseFailureRecorded(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{})
	bad := []byte(header + "payment,ref1,abc,TWD,0,2026-08-19T00:00:00Z\n")

	_, err := svc.ImportSettlementFile(context.Background(), importCmd(bad))
	require.Error(t, err)
	require.ErrorIs(t, err, domain.ErrParse)
	var pe *domain.ParseError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 1, pe.Line)

	require.Len(t, store.Files, 1)
	for _, f := range store.Files {
		assert.Equal(t, domain.FileFailed, f.Status)
		assert.Contains(t, f.Error, "amount_minor")
		assert.Equal(t, domain.FileHash(bad), f.FileHash)
	}
	assert.Empty(t, store.Runs)
	assert.Empty(t, store.Outbox)

	// 再匯入同一壞檔：仍失敗、不是 already_imported、檔案仍只有一筆。
	_, err = svc.ImportSettlementFile(context.Background(), importCmd(bad))
	require.ErrorIs(t, err, domain.ErrParse)
	assert.Len(t, store.Files, 1)

	// 空檔。
	_, err = svc.ImportSettlementFile(context.Background(), importCmd([]byte(header)))
	assert.ErrorIs(t, err, domain.ErrEmptyFile)
}

func TestImportSettlementFile_RetryAfterFailure(t *testing.T) {
	// 先前 failed 的檔案（例如 parser 修正後）再次匯入要能成功並建立 run。
	svc, store, _ := newSvc(t, app.Config{})
	content := fixture(t)
	failed := domain.NewSettlementFile(domain.MockProvider, "mock.csv", domain.FileHash(content), nil, nil, now.Add(-time.Hour))
	failed.MarkFailed("old parser bug", now.Add(-time.Hour))
	store.Files[failed.ID] = failed

	res, err := svc.ImportSettlementFile(context.Background(), importCmd(content))
	require.NoError(t, err)
	assert.False(t, res.AlreadyImported)
	assert.Equal(t, failed.ID, res.File.ID)
	assert.Equal(t, domain.FileImported, res.File.Status)
	assert.Empty(t, res.File.Error)
	assert.Len(t, store.Runs, 1)
}

func TestImportSettlementFile_CrashRecoveryUsesExistingLines(t *testing.T) {
	// 檔案 imported、lines 已在、但沒有 run：重跑只做比對，不重複插入 lines。
	svc, store, _ := newSvc(t, app.Config{})
	content := fixture(t)
	file := domain.NewSettlementFile(domain.MockProvider, "mock.csv", domain.FileHash(content), nil, nil, now)
	file.MarkImported(6, now)
	store.Files[file.ID] = file
	lines, err := domain.NewMockParser().Parse(bytesReader(content))
	require.NoError(t, err)
	for i := range lines {
		lines[i].ID = ids.NewUUID()
		lines[i].FileID = file.ID
	}
	store.Lines[file.ID] = lines

	res, err := svc.ImportSettlementFile(context.Background(), importCmd(content))
	require.NoError(t, err)
	assert.False(t, res.AlreadyImported)
	assert.Equal(t, domain.RunCompleted, res.Run.Status)
	assert.Len(t, store.Lines[file.ID], 6)
	assert.Equal(t, lines[0].ID, store.Lines[file.ID][0].ID, "沿用既有 line id")
}

func TestImportSettlementFile_Validation(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{})
	ctx := context.Background()
	content := fixture(t)

	t.Run("unknown format", func(t *testing.T) {
		cmd := importCmd(content)
		cmd.Format = "adyen"
		_, err := svc.ImportSettlementFile(ctx, cmd)
		assert.ErrorIs(t, err, domain.ErrUnknownFormat)
	})
	t.Run("format provider mismatch", func(t *testing.T) {
		cmd := importCmd(content)
		cmd.Provider = "stripe"
		_, err := svc.ImportSettlementFile(ctx, cmd)
		assert.ErrorIs(t, err, domain.ErrUnknownFormat)
	})
	t.Run("missing provider", func(t *testing.T) {
		cmd := importCmd(content)
		cmd.Provider = ""
		_, err := svc.ImportSettlementFile(ctx, cmd)
		assert.ErrorIs(t, err, apperr.ErrParameterMissing)
	})
	t.Run("missing file name", func(t *testing.T) {
		cmd := importCmd(content)
		cmd.FileName = ""
		_, err := svc.ImportSettlementFile(ctx, cmd)
		assert.ErrorIs(t, err, apperr.ErrParameterMissing)
	})
	t.Run("bad settlement date", func(t *testing.T) {
		cmd := importCmd(content)
		cmd.SettlementDate = "2026/08/19"
		_, err := svc.ImportSettlementFile(ctx, cmd)
		assert.ErrorIs(t, err, domain.ErrInvalidPeriod)
	})
	t.Run("checksum mismatch", func(t *testing.T) {
		cmd := importCmd(content)
		cmd.ExpectedChecksum = "deadbeef"
		_, err := svc.ImportSettlementFile(ctx, cmd)
		assert.ErrorIs(t, err, apperr.ErrParameterInvalid)
	})
	t.Run("checksum match case-insensitive", func(t *testing.T) {
		cmd := importCmd(content)
		cmd.ExpectedChecksum = toUpper(domain.FileHash(content))
		_, err := svc.ImportSettlementFile(ctx, cmd)
		require.NoError(t, err)
	})
	assert.Len(t, store.Files, 1, "驗證失敗不留任何檔案")
}

func TestImportSettlementFile_SuppressesDuplicateOpenDiscrepancies(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{})
	// 先匯入一次 → G5 missing_in_ledger open。
	_, err := svc.ImportSettlementFile(context.Background(), importCmd(fixture(t)))
	require.NoError(t, err)
	before := len(store.Discrepancies)
	assert.Positive(t, before)

	// 第二份檔案（不同 hash，多一列）含同樣的 G5：不重複開單。
	content := append([]byte(nil), fixture(t)...)
	content = append(content, []byte("payment,mock_ch_NEW,5,TWD,0,2026-08-20T00:00:00Z\n")...)
	cmd := importCmd(content)
	cmd.SettlementDate = "2026-08-20"
	res, err := svc.ImportSettlementFile(context.Background(), cmd)
	require.NoError(t, err)
	assert.Len(t, store.Discrepancies, before+1, "只有 mock_ch_NEW 是新差異")
	assert.Equal(t, before, res.Run.Summary.Suppressed)
	assert.Equal(t, before+1, res.Run.UnmatchedCount, "run 仍記錄實際發現的差異數")
}

func TestImportSettlementFile_RepoErrorRollsBack(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{})
	store.Errs["discrepancies.insert"] = errors.New("db down")
	_, err := svc.ImportSettlementFile(context.Background(), importCmd(fixture(t)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
	assert.Empty(t, store.OutboxByType(app.EventRunCompleted))

	// fake 無真正 rollback，改用新的 store 驗證 outbox 失敗也會上拋。
	svc2, store2, _ := newSvc(t, app.Config{})
	store2.Errs["outbox.insert"] = errors.New("outbox down")
	_, err = svc2.ImportSettlementFile(context.Background(), importCmd(fixture(t)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outbox down")
}

func TestGetAndListRuns(t *testing.T) {
	svc, store, clock := newSvc(t, app.Config{})
	ctx := context.Background()
	r1, err := svc.ImportSettlementFile(ctx, importCmd(fixture(t)))
	require.NoError(t, err)
	clock.Advance(time.Minute)
	cmd := importCmd([]byte(header + "payment,x,1,TWD,0,2026-08-20T00:00:00Z\n"))
	cmd.SettlementDate = "2026-08-20"
	r2, err := svc.ImportSettlementFile(ctx, cmd)
	require.NoError(t, err)

	got, err := svc.GetReconciliationRun(ctx, r1.Run.PublicID)
	require.NoError(t, err)
	assert.Equal(t, r1.Run.ID, got.ID)
	_, err = svc.GetReconciliationRun(ctx, "rr_nope")
	require.ErrorIs(t, err, domain.ErrRunNotFound)
	_, err = svc.GetReconciliationRun(ctx, ids.New(domain.PrefixRun))
	require.ErrorIs(t, err, domain.ErrRunNotFound)

	runs, next, err := svc.ListReconciliationRuns(ctx, app.RunFilter{PageSize: 1})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, r2.Run.ID, runs[0].ID, "created_at DESC")
	assert.NotEmpty(t, next)
	runs, next, err = svc.ListReconciliationRuns(ctx, app.RunFilter{PageSize: 1, PageToken: next})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, r1.Run.ID, runs[0].ID)
	assert.Empty(t, next)

	runs, _, err = svc.ListReconciliationRuns(ctx, app.RunFilter{DateFrom: mustDate("2026-08-20")})
	require.NoError(t, err)
	assert.Len(t, runs, 1)
	runs, _, err = svc.ListReconciliationRuns(ctx, app.RunFilter{Statuses: []domain.RunStatus{domain.RunFailed}})
	require.NoError(t, err)
	assert.Empty(t, runs)
	runs, _, err = svc.ListReconciliationRuns(ctx, app.RunFilter{Provider: "stripe"})
	require.NoError(t, err)
	assert.Empty(t, runs)
	_ = store
}

func TestListDiscrepancies(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{})
	ctx := context.Background()
	store.AddRecord(rec(domain.RecordPayment, "mock_ch_01K2W3X4Y5Z6A7B8C9D0E1F2G4", domain.StatusCaptured, 1, now.Add(-30*24*time.Hour)))
	res, err := svc.ImportSettlementFile(ctx, importCmd(fixture(t)))
	require.NoError(t, err)

	all, _, err := svc.ListDiscrepancies(ctx, app.DiscrepancyFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 5, "G3 G5 H1 J1 missing_in_ledger + G4 amount_mismatch")

	runID := res.Run.ID
	byRun, _, err := svc.ListDiscrepancies(ctx, app.DiscrepancyFilter{RunID: &runID})
	require.NoError(t, err)
	assert.Len(t, byRun, 5)

	amt, _, err := svc.ListDiscrepancies(ctx, app.DiscrepancyFilter{Kinds: []domain.DiscrepancyKind{domain.KindAmountMismatch}})
	require.NoError(t, err)
	require.Len(t, amt, 1)
	assert.Equal(t, int64(1), *amt[0].ExpectedAmount)
	assert.Equal(t, int64(250000), *amt[0].ActualAmount)

	byMerchant, _, err := svc.ListDiscrepancies(ctx, app.DiscrepancyFilter{MerchantID: &merchantID})
	require.NoError(t, err)
	assert.Len(t, byMerchant, 1)

	open, _, err := svc.ListDiscrepancies(ctx, app.DiscrepancyFilter{Statuses: []domain.DiscrepancyStatus{domain.DiscrepancyResolved}})
	require.NoError(t, err)
	assert.Empty(t, open)

	_, _, err = svc.ListDiscrepancies(ctx, app.DiscrepancyFilter{Kinds: []domain.DiscrepancyKind{"bogus"}})
	require.ErrorIs(t, err, apperr.ErrParameterInvalid)

	page1, next, err := svc.ListDiscrepancies(ctx, app.DiscrepancyFilter{PageSize: 2})
	require.NoError(t, err)
	assert.Len(t, page1, 2)
	assert.NotEmpty(t, next)
	page2, next2, err := svc.ListDiscrepancies(ctx, app.DiscrepancyFilter{PageSize: 200, PageToken: next})
	require.NoError(t, err)
	assert.Len(t, page2, 3)
	assert.Empty(t, next2)
}

func TestResolveDiscrepancy(t *testing.T) {
	svc, store, _ := newSvc(t, app.Config{})
	ctx := context.Background()
	res, err := svc.ImportSettlementFile(ctx, importCmd(fixture(t)))
	require.NoError(t, err)
	dscIDs := make([]string, 0, len(store.Discrepancies))
	for _, d := range store.Discrepancies {
		dscIDs = append(dscIDs, d.PublicID())
	}
	require.GreaterOrEqual(t, len(dscIDs), 2)

	t.Run("resolve", func(t *testing.T) {
		d, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: dscIDs[0], Action: app.ActionResolve, Note: "fixed", ResolvedBy: "ops:alice", IdempotencyKey: "k1"})
		require.NoError(t, err)
		assert.Equal(t, domain.DiscrepancyResolved, d.Status)
		assert.Equal(t, "ops:alice", d.ResolvedBy)
		assert.Equal(t, "k1", d.Detail(domain.DetailIdempotencyKey))
		require.NotNil(t, d.ResolvedAt)

		evs := store.OutboxByType(app.EventDiscrepancyResolved)
		require.Len(t, evs, 1)
		var ev app.DiscrepancyResolvedEvent
		require.NoError(t, json.Unmarshal(evs[0].Payload, &ev))
		assert.Equal(t, d.PublicID(), ev.DiscrepancyID)
		assert.Equal(t, res.Run.PublicID, ev.RunID)
		assert.Equal(t, "resolved", ev.Status)
	})
	t.Run("idempotent replay", func(t *testing.T) {
		d, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: dscIDs[0], Action: app.ActionResolve, Note: "fixed", ResolvedBy: "ops:alice", IdempotencyKey: "k1"})
		require.NoError(t, err)
		assert.Equal(t, domain.DiscrepancyResolved, d.Status)
		assert.Len(t, store.OutboxByType(app.EventDiscrepancyResolved), 1, "重送不再發事件")
	})
	t.Run("already resolved with different key", func(t *testing.T) {
		_, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: dscIDs[0], Action: app.ActionIgnore, Note: "x", ResolvedBy: "ops:bob", IdempotencyKey: "k2"})
		assert.ErrorIs(t, err, domain.ErrInvalidTransition)
	})
	t.Run("ignore requires note", func(t *testing.T) {
		_, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: dscIDs[1], Action: app.ActionIgnore, ResolvedBy: "ops:bob"})
		assert.ErrorIs(t, err, domain.ErrResolutionNoteRequired)
	})
	t.Run("resolved_by required", func(t *testing.T) {
		_, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: dscIDs[1], Action: app.ActionResolve})
		assert.ErrorIs(t, err, domain.ErrResolvedByRequired)
	})
	t.Run("ignore", func(t *testing.T) {
		d, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: dscIDs[1], Action: app.ActionIgnore, Note: "T+2", ResolvedBy: "ops:bob"})
		require.NoError(t, err)
		assert.Equal(t, domain.DiscrepancyIgnored, d.Status)
	})
	t.Run("unknown action", func(t *testing.T) {
		d := domain.Discrepancy{ID: ids.NewUUID(), RunID: res.Run.ID, Kind: domain.KindMissingInPSP, Provider: "mock", Status: domain.DiscrepancyOpen}
		store.AddDiscrepancy(d)
		_, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: d.PublicID(), Action: "adjust", ResolvedBy: "x"})
		assert.ErrorIs(t, err, apperr.ErrParameterInvalid)
	})
	t.Run("not found", func(t *testing.T) {
		_, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: ids.New(domain.PrefixDiscrepancy), Action: app.ActionResolve, ResolvedBy: "x"})
		require.ErrorIs(t, err, domain.ErrDiscrepancyNotFound)
		_, err = svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: "garbage", Action: app.ActionResolve, ResolvedBy: "x"})
		assert.ErrorIs(t, err, domain.ErrDiscrepancyNotFound)
	})
	t.Run("optimistic lock conflict surfaces", func(t *testing.T) {
		d := domain.Discrepancy{ID: ids.NewUUID(), RunID: res.Run.ID, Kind: domain.KindMissingInPSP, Provider: "mock", Status: domain.DiscrepancyOpen}
		store.AddDiscrepancy(d)
		store.FailNextUpdate.Store(true)
		_, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: d.PublicID(), Action: app.ActionResolve, ResolvedBy: "x"})
		assert.ErrorIs(t, err, domain.ErrConcurrentModification)
	})
}

// ---- helpers ----

func mustDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func toUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

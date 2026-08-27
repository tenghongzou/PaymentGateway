//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/adapter/postgres"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/migrations"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

var (
	pool *pgxpool.Pool
	now  = time.Date(2026, 8, 20, 6, 0, 0, 0, time.UTC)
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("pg_recon"), tcpostgres.WithUsername("recon"), tcpostgres.WithPassword("recon"),
		tcpostgres.BasicWaitStrategies())
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		os.Exit(1)
	}
	url, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "conn string:", err)
		os.Exit(1)
	}
	src, err := migrations.Source("reconciliation")
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrations:", err)
		os.Exit(1)
	}
	if err = pgdb.Migrate(ctx, url, "reconciliation", src); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	pool, err = pgdb.Connect(ctx, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	code := m.Run()
	pool.Close()
	if err = testcontainers.TerminateContainer(pg); err != nil {
		fmt.Fprintln(os.Stderr, "terminate postgres:", err)
	}
	os.Exit(code)
}

func truncateAll(t *testing.T) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `TRUNCATE discrepancies, reconciliation_runs, settlement_lines, settlement_files, payment_records, ledger_postings, outbox, processed_events`)
	require.NoError(t, err)
}

func record(kind domain.RecordKind, ref, status string, amount int64, occurred time.Time) domain.PaymentRecord {
	u := ids.NewUUID()
	return domain.PaymentRecord{
		ID: u, Kind: kind, PublicID: ids.Format(string(kind)[:2], u), MerchantID: ids.NewUUID(), Provider: "mock",
		ProviderReference: ref, Amount: money.MustNew(amount, "TWD"), Status: status, OccurredAt: occurred, SourceSeq: 1,
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestFileRepo(t *testing.T) {
	truncateAll(t)
	ctx := context.Background()
	repos := postgres.NewRepos(pool)
	start := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	f := domain.NewSettlementFile("mock", "a.csv", domain.FileHash([]byte("a")), &start, &end, now)
	f.Metadata["format"] = "mock_csv"
	require.NoError(t, repos.FileRepo.Create(ctx, f))

	dup := domain.NewSettlementFile("mock", "b.csv", f.FileHash, nil, nil, now)
	require.ErrorIs(t, repos.FileRepo.Create(ctx, dup), domain.ErrDuplicateFile)

	got, err := repos.GetByHash(ctx, f.FileHash)
	require.NoError(t, err)
	assert.Equal(t, f.ID, got.ID)
	assert.Equal(t, domain.FileImporting, got.Status)
	assert.Equal(t, "mock_csv", got.Metadata["format"])
	require.NotNil(t, got.PeriodStart)
	assert.Equal(t, "2026-08-19", got.PeriodStart.Format("2006-01-02"))
	assert.Equal(t, 0, got.Version)

	got.MarkImported(3, now)
	require.NoError(t, repos.FileRepo.Update(ctx, got))
	assert.Equal(t, 1, got.Version)
	stale := *f
	stale.MarkFailed("x", now)
	require.ErrorIs(t, repos.FileRepo.Update(ctx, &stale), domain.ErrConcurrentModification)

	byID, err := repos.FileRepo.GetByID(ctx, f.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.FileImported, byID.Status)
	assert.Equal(t, 3, byID.RowCount)
	require.NotNil(t, byID.ImportedAt)

	_, err = repos.GetByHash(ctx, "nope")
	require.ErrorIs(t, err, domain.ErrFileNotFound)
	_, err = repos.FileRepo.GetByID(ctx, ids.NewUUID())
	assert.ErrorIs(t, err, domain.ErrFileNotFound)
}

func TestLineRepo(t *testing.T) {
	truncateAll(t)
	ctx := context.Background()
	repos := postgres.NewRepos(pool)
	f := domain.NewSettlementFile("mock", "a.csv", domain.FileHash([]byte("lines")), nil, nil, now)
	require.NoError(t, repos.FileRepo.Create(ctx, f))

	content, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "test", "fixtures", "settlement", "mock-2026-08-19.csv"))
	require.NoError(t, err)
	lines, err := domain.NewMockParser().Parse(bytesReader(content))
	require.NoError(t, err)
	for i := range lines {
		lines[i].ID = ids.NewUUID()
		lines[i].FileID = f.ID
		lines[i].CreatedAt = now
	}
	require.NoError(t, repos.LineRepo.InsertBatch(ctx, lines))
	// 重跑：(file_id, line_no) 衝突略過。
	require.NoError(t, repos.LineRepo.InsertBatch(ctx, lines))

	got, err := repos.ListByFile(ctx, f.ID)
	require.NoError(t, err)
	require.Len(t, got, 6)
	assert.Equal(t, lines[0].ID, got[0].ID)
	assert.Equal(t, int64(59), got[0].Fee.AmountMinor, "fee 存在 raw.fee_minor")
	assert.Equal(t, "TWD", got[0].Fee.Currency)
	assert.Equal(t, "59", got[0].Raw["fee_minor"])
	assert.Equal(t, domain.LineChargeback, got[4].Type)
	assert.Equal(t, lines[0].SettledAt, got[0].SettledAt.UTC())

	// CHECK：不合法 type 被 DB 擋下。
	bad := lines[0]
	bad.ID, bad.LineNo, bad.Type = ids.NewUUID(), 99, "payout"
	assert.Error(t, repos.LineRepo.InsertBatch(ctx, []domain.SettlementLine{bad}))
}

func TestPaymentRecordRepo(t *testing.T) {
	truncateAll(t)
	ctx := context.Background()
	repos := postgres.NewRepos(pool)
	old := now.Add(-5 * 24 * time.Hour)

	r := record(domain.RecordPayment, "mock_ch_1", domain.StatusAuthorized, 1000, old)
	applied, err := repos.Upsert(ctx, &r)
	require.NoError(t, err)
	assert.True(t, applied)

	// 較新 seq：套用。
	r.Status, r.SourceSeq = domain.StatusCaptured, 2
	applied, err = repos.Upsert(ctx, &r)
	require.NoError(t, err)
	assert.True(t, applied)

	// 較舊 seq：不套用。
	stale := r
	stale.Status, stale.SourceSeq = domain.StatusVoided, 1
	applied, err = repos.Upsert(ctx, &stale)
	require.NoError(t, err)
	assert.False(t, applied)

	got, err := repos.Get(ctx, r.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCaptured, got.Status)
	assert.Equal(t, 2, got.SourceSeq)
	assert.Nil(t, got.Fee, "fee 未持久化（migration TODO）")

	// provider_reference 為空的更新不會抹掉既有值（COALESCE）。
	noRef := r
	noRef.ProviderReference, noRef.SourceSeq = "", 3
	_, err = repos.Upsert(ctx, &noRef)
	require.NoError(t, err)
	got, err = repos.Get(ctx, r.ID)
	require.NoError(t, err)
	assert.Equal(t, "mock_ch_1", got.ProviderReference)

	missing, err := repos.Get(ctx, ids.NewUUID())
	require.NoError(t, err)
	assert.Nil(t, missing)

	refund := record(domain.RecordRefund, "mock_re_1", domain.RefundSucceeded, 300, old)
	pending := record(domain.RecordRefund, "mock_re_2", domain.RefundPending, 300, old)
	recent := record(domain.RecordPayment, "mock_ch_recent", domain.StatusCaptured, 5, now.Add(-time.Hour))
	other := record(domain.RecordPayment, "stripe_ch", domain.StatusCaptured, 5, old)
	other.Provider = "stripe"
	for _, x := range []*domain.PaymentRecord{&refund, &pending, &recent, &other} {
		_, err = repos.Upsert(ctx, x)
		require.NoError(t, err)
	}

	found, err := repos.FindByProviderRefs(ctx, "mock", []string{"mock_ch_1", "mock_re_1", "zzz"})
	require.NoError(t, err)
	assert.Len(t, found, 2)
	found, err = repos.FindByProviderRefs(ctx, "mock", nil)
	require.NoError(t, err)
	assert.Empty(t, found)

	// 未結算：captured 付款 + succeeded 退款；pending 退款、近期、其他 provider 不算。
	unsettled, err := repos.ListUnsettled(ctx, "mock", now.Add(-72*time.Hour), 100)
	require.NoError(t, err)
	refs := map[string]bool{}
	for _, u := range unsettled {
		refs[u.ProviderReference] = true
	}
	assert.Equal(t, map[string]bool{"mock_ch_1": true, "mock_re_1": true}, refs)

	// 插入一條對應的結算列後，mock_ch_1 不再是未結算（本地 JOIN）。
	f := domain.NewSettlementFile("mock", "a.csv", domain.FileHash([]byte("u")), nil, nil, now)
	require.NoError(t, repos.FileRepo.Create(ctx, f))
	require.NoError(t, repos.LineRepo.InsertBatch(ctx, []domain.SettlementLine{{
		ID: ids.NewUUID(), FileID: f.ID, LineNo: 1, Provider: "mock", ProviderReference: "mock_ch_1", Type: domain.LinePayment,
		Amount: money.MustNew(1000, "TWD"), Fee: money.MustNew(59, "TWD"), SettledAt: now, CreatedAt: now,
	}}))
	unsettled, err = repos.ListUnsettled(ctx, "mock", now.Add(-72*time.Hour), 100)
	require.NoError(t, err)
	require.Len(t, unsettled, 1)
	assert.Equal(t, "mock_re_1", unsettled[0].ProviderReference)

	// limit。
	unsettled, err = repos.ListUnsettled(ctx, "mock", now, 1)
	require.NoError(t, err)
	assert.Len(t, unsettled, 1)
}

func TestRunAndDiscrepancyRepo(t *testing.T) {
	truncateAll(t)
	ctx := context.Background()
	repos := postgres.NewRepos(pool)
	start := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	fileID := ids.NewUUID()

	run, err := domain.NewRun("mock", start, start.Add(24*time.Hour), "ops:test", now)
	require.NoError(t, err)
	run.Summary.FileID = fileID.String()
	require.NoError(t, repos.RunRepo.Create(ctx, run))

	byFile, err := repos.FindByFileID(ctx, fileID)
	require.NoError(t, err)
	require.NotNil(t, byFile)
	assert.Equal(t, run.ID, byFile.ID)
	none, err := repos.FindByFileID(ctx, ids.NewUUID())
	require.NoError(t, err)
	assert.Nil(t, none)

	require.NoError(t, run.Start(now))
	require.NoError(t, repos.RunRepo.Update(ctx, run))

	// 比對結果 → discrepancies（含 fee_mismatch 的 CHECK 對應）。
	line := domain.SettlementLine{ID: ids.NewUUID(), LineNo: 1, Provider: "mock", ProviderReference: "mock_ch_1", Type: domain.LinePayment, Amount: money.MustNew(1000, "TWD"), Fee: money.MustNew(60, "TWD"), SettledAt: now}
	fee := money.MustNew(59, "TWD")
	rec := record(domain.RecordPayment, "mock_ch_1", domain.StatusCaptured, 1000, now.Add(-5*24*time.Hour))
	rec.Fee = &fee
	missing := record(domain.RecordPayment, "mock_ch_missing", domain.StatusCaptured, 7, now.Add(-5*24*time.Hour))
	res := domain.NewMatcher().Match(domain.MatchInput{Provider: "mock", Lines: []domain.SettlementLine{line,
		{ID: ids.NewUUID(), LineNo: 2, Provider: "mock", ProviderReference: "mock_ch_x", Type: domain.LinePayment, Amount: money.MustNew(1, "TWD"), SettledAt: now},
	}, Records: []domain.PaymentRecord{rec, missing}, Now: now})
	require.Len(t, res.Discrepancies, 3)
	// settlement_line_id 有 FK：沒有真正寫入 lines，故清掉。
	for i := range res.Discrepancies {
		res.Discrepancies[i].RunID = run.ID
		res.Discrepancies[i].SettlementLineID = nil
		res.Discrepancies[i].CreatedAt = now.Add(time.Duration(i) * time.Second)
		res.Discrepancies[i].UpdatedAt = res.Discrepancies[i].CreatedAt
	}
	require.NoError(t, repos.DiscrepancyRepo.InsertBatch(ctx, res.Discrepancies))
	require.NoError(t, run.Complete(res, now.Add(time.Second)))
	require.NoError(t, repos.RunRepo.Update(ctx, run))

	got, err := repos.RunRepo.GetByID(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.RunCompleted, got.Status)
	assert.Equal(t, 3, got.UnmatchedCount)
	assert.Equal(t, map[string]int{"fee_mismatch": 1, "missing_in_ledger": 1, "missing_in_psp": 1}, got.Summary.ByKind)
	assert.Equal(t, int64(1001), got.Summary.TotalSettled["TWD"])
	assert.Equal(t, run.Version, got.Version)

	// 樂觀鎖：用舊版本更新 → 衝突。
	stale := *got
	stale.Version = got.Version - 1
	require.ErrorIs(t, repos.RunRepo.Update(ctx, &stale), domain.ErrConcurrentModification)
	_, err = repos.RunRepo.GetByID(ctx, ids.NewUUID())
	require.ErrorIs(t, err, domain.ErrRunNotFound)

	// fee_mismatch 讀回仍是 fee_mismatch；DB 內 kind 為 amount_mismatch。
	all, next, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{PageSize: 10})
	require.NoError(t, err)
	assert.Empty(t, next)
	require.Len(t, all, 3)
	kinds := map[string]int{}
	for _, d := range all {
		kinds[string(d.Kind)]++
	}
	assert.Equal(t, map[string]int{"fee_mismatch": 1, "missing_in_ledger": 1, "missing_in_psp": 1}, kinds)
	var dbKind string
	var details []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT kind, details FROM discrepancies WHERE details->>'kind' = 'fee_mismatch'`).Scan(&dbKind, &details))
	assert.Equal(t, "amount_mismatch", dbKind)
	var dm map[string]any
	require.NoError(t, json.Unmarshal(details, &dm))
	assert.Equal(t, "fee_mismatch", dm["kind"])

	feeOnly, _, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{Kinds: []domain.DiscrepancyKind{domain.KindFeeMismatch}})
	require.NoError(t, err)
	require.Len(t, feeOnly, 1)
	assert.Equal(t, int64(59), *feeOnly[0].ExpectedAmount)
	assert.Equal(t, int64(60), *feeOnly[0].ActualAmount)
	snap, ok := feeOnly[0].LineSnapshot()
	require.True(t, ok)
	assert.Equal(t, "mock_ch_1", snap.ProviderReference)
	amountOnly, _, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{Kinds: []domain.DiscrepancyKind{domain.KindAmountMismatch}})
	require.NoError(t, err)
	assert.Empty(t, amountOnly, "amount_mismatch 篩選不含 fee_mismatch")
	both, _, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{Kinds: []domain.DiscrepancyKind{domain.KindAmountMismatch, domain.KindFeeMismatch, domain.KindMissingInPSP}})
	require.NoError(t, err)
	assert.Len(t, both, 2)

	// 其他篩選。
	runID := run.ID
	byRun, _, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{RunID: &runID, Statuses: []domain.DiscrepancyStatus{domain.DiscrepancyOpen}})
	require.NoError(t, err)
	assert.Len(t, byRun, 3)
	mid := rec.MerchantID
	byMerchant, _, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{MerchantID: &mid})
	require.NoError(t, err)
	assert.Len(t, byMerchant, 1)
	byPay, _, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{PaymentID: missing.PublicID})
	require.NoError(t, err)
	require.Len(t, byPay, 1)
	assert.Equal(t, domain.KindMissingInPSP, byPay[0].Kind)
	after, _, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{CreatedAfter: now.Add(500 * time.Millisecond)})
	require.NoError(t, err)
	assert.Len(t, after, 2)

	// keyset 分頁。
	p1, tok, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{PageSize: 2})
	require.NoError(t, err)
	require.Len(t, p1, 2)
	require.NotEmpty(t, tok)
	p2, tok2, err := repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{PageSize: 2, PageToken: tok})
	require.NoError(t, err)
	require.Len(t, p2, 1)
	assert.Empty(t, tok2)
	assert.True(t, p1[0].CreatedAt.After(p1[1].CreatedAt) && p1[1].CreatedAt.After(p2[0].CreatedAt), "created_at DESC")
	_, _, err = repos.DiscrepancyRepo.List(ctx, app.DiscrepancyFilter{PageToken: "!!"})
	require.Error(t, err)

	// ExistsOpen。
	exists, err := repos.ExistsOpen(ctx, "mock", domain.KindFeeMismatch, "mock_ch_1", "")
	require.NoError(t, err)
	assert.True(t, exists)
	exists, err = repos.ExistsOpen(ctx, "mock", domain.KindAmountMismatch, "mock_ch_1", "")
	require.NoError(t, err)
	assert.False(t, exists)
	exists, err = repos.ExistsOpen(ctx, "mock", domain.KindMissingInPSP, "", missing.PublicID)
	require.NoError(t, err)
	assert.True(t, exists)
	exists, err = repos.ExistsOpen(ctx, "stripe", domain.KindMissingInPSP, "", missing.PublicID)
	require.NoError(t, err)
	assert.False(t, exists)

	// Resolve → Update（含 resolved_at CHECK、樂觀鎖）。
	d := feeOnly[0]
	require.NoError(t, d.Resolve("ok", "ops:a", now))
	require.NoError(t, repos.DiscrepancyRepo.Update(ctx, &d))
	got2, err := repos.DiscrepancyRepo.GetByID(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.DiscrepancyResolved, got2.Status)
	assert.Equal(t, "ops:a", got2.ResolvedBy)
	require.NotNil(t, got2.ResolvedAt)
	assert.Equal(t, domain.KindFeeMismatch, got2.Kind)
	assert.Equal(t, 1, got2.Version)
	// 同版本再更新 → 衝突。
	require.ErrorIs(t, repos.DiscrepancyRepo.Update(ctx, &d), domain.ErrConcurrentModification)
	exists, err = repos.ExistsOpen(ctx, "mock", domain.KindFeeMismatch, "mock_ch_1", "")
	require.NoError(t, err)
	assert.False(t, exists, "resolved 不算 open")
	_, err = repos.DiscrepancyRepo.GetByID(ctx, ids.NewUUID())
	require.ErrorIs(t, err, domain.ErrDiscrepancyNotFound)

	// CHECK：status=resolved 但 resolved_at NULL 會被 DB 擋下（繞過 domain）。
	_, err = pool.Exec(ctx, `UPDATE discrepancies SET status = 'ignored', resolved_at = NULL WHERE id = $1`, d.ID)
	assert.True(t, pgdb.IsCheckViolation(err))

	// Run list。
	run2, err := domain.NewRun("mock", start.Add(24*time.Hour), start.Add(48*time.Hour), "scheduler", now.Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, repos.RunRepo.Create(ctx, run2))
	runs, tok, err := repos.RunRepo.List(ctx, app.RunFilter{PageSize: 1})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, run2.ID, runs[0].ID)
	require.NotEmpty(t, tok)
	runs, tok, err = repos.RunRepo.List(ctx, app.RunFilter{PageSize: 1, PageToken: tok})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, run.ID, runs[0].ID)
	assert.Empty(t, tok)
	runs, _, err = repos.RunRepo.List(ctx, app.RunFilter{Statuses: []domain.RunStatus{domain.RunCompleted}, Provider: "mock"})
	require.NoError(t, err)
	assert.Len(t, runs, 1)
	runs, _, err = repos.RunRepo.List(ctx, app.RunFilter{DateFrom: start.Add(24 * time.Hour), DateTo: start.Add(24 * time.Hour)})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, run2.ID, runs[0].ID)
}

func TestTxManagerOutboxInbox(t *testing.T) {
	truncateAll(t)
	ctx := context.Background()
	repos := postgres.NewRepos(pool)

	// 交易外使用 outbox / inbox → 錯誤。
	_, err := repos.Insert(ctx, app.OutboxMessage{AggregateType: "x", AggregateID: "y", EventType: "z", Payload: []byte("{}")})
	require.Error(t, err)
	_, err = repos.MarkProcessed(ctx, ids.NewUUID().String(), "c")
	require.Error(t, err)

	eventID := ids.NewUUID().String()
	err = repos.Tx.WithinTx(ctx, func(ctx context.Context) error {
		already, markErr := repos.MarkProcessed(ctx, eventID, "recon.test")
		if markErr != nil {
			return markErr
		}
		assert.False(t, already)
		// 巢狀重用同一交易。
		return repos.Tx.WithinTx(ctx, func(ctx context.Context) error {
			again, nestedErr := repos.MarkProcessed(ctx, eventID, "recon.test")
			if nestedErr != nil {
				return nestedErr
			}
			assert.True(t, again)
			_, insErr := repos.Insert(ctx, app.OutboxMessage{AggregateType: "reconciliation_run", AggregateID: "rr_1", EventType: "reconciliation.run.completed", Payload: []byte(`{"a":1}`), Headers: map[string]string{"content-type": "application/json"}})
			return insErr
		})
	})
	require.NoError(t, err)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type = 'reconciliation.run.completed' AND headers->>'content-type' = 'application/json'`).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM processed_events WHERE consumer = 'recon.test'`).Scan(&n))
	assert.Equal(t, 1, n)

	// 回滾：fn 回錯誤 → 什麼都不留。
	boom := errors.New("boom")
	err = repos.Tx.WithinTx(ctx, func(ctx context.Context) error {
		_, markErr := repos.MarkProcessed(ctx, ids.NewUUID().String(), "rollback")
		require.NoError(t, markErr)
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM processed_events WHERE consumer = 'rollback'`).Scan(&n))
	assert.Equal(t, 0, n)
}

// TestEndToEndImport 以真 DB 跑完整 use case：匯入 fixture → run / lines / discrepancies / outbox 全部落地。
func TestEndToEndImport(t *testing.T) {
	truncateAll(t)
	ctx := context.Background()
	repos := postgres.NewRepos(pool)
	deps := repos.Deps()
	deps.Clock = app.ClockFunc(func() time.Time { return now })
	svc := app.NewService(deps, app.Config{GracePeriod: 72 * time.Hour})

	old := now.Add(-5 * 24 * time.Hour)
	for _, r := range []domain.PaymentRecord{
		record(domain.RecordPayment, "mock_ch_01K2W3X4Y5Z6A7B8C9D0E1F2G3", domain.StatusCaptured, 1000, old),
		record(domain.RecordPayment, "mock_ch_01K2W3X4Y5Z6A7B8C9D0E1F2G4", domain.StatusCaptured, 250001, old),
		record(domain.RecordRefund, "mock_re_01K2W3X4Y5Z6A7B8C9D0E1F2H1", domain.RefundSucceeded, 300, old),
		record(domain.RecordDispute, "mock_du_01K2W3X4Y5Z6A7B8C9D0E1F2J1", domain.DisputeLost, 1000, old),
		record(domain.RecordPayment, "mock_ch_OLD", domain.StatusCaptured, 77, old),
	} {
		rr := r
		_, err := repos.Upsert(ctx, &rr)
		require.NoError(t, err)
	}
	content, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "test", "fixtures", "settlement", "mock-2026-08-19.csv"))
	require.NoError(t, err)
	cmd := app.ImportCommand{Provider: "mock", Format: domain.FormatMockCSV, FileName: "mock-2026-08-19.csv", Content: content, SettlementDate: "2026-08-19", TriggeredBy: "it"}

	res, err := svc.ImportSettlementFile(ctx, cmd)
	require.NoError(t, err)
	assert.False(t, res.AlreadyImported)
	assert.Equal(t, domain.RunCompleted, res.Run.Status)
	assert.Equal(t, 3, res.Run.MatchedCount)
	assert.Equal(t, 3, res.Run.UnmatchedCount)

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM settlement_lines WHERE file_id = $1`, res.File.ID).Scan(&n))
	assert.Equal(t, 6, n)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM discrepancies WHERE run_id = $1 AND settlement_line_id IS NOT NULL`, res.Run.ID).Scan(&n))
	assert.Equal(t, 2, n, "amount_mismatch + missing_in_ledger 帶 line id")
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type = 'settlement.posted'`).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type = 'reconciliation.run.completed' AND aggregate_id = $1`, res.Run.PublicID).Scan(&n))
	assert.Equal(t, 1, n)

	// 冪等。
	again, err := svc.ImportSettlementFile(ctx, cmd)
	require.NoError(t, err)
	assert.True(t, again.AlreadyImported)
	assert.Equal(t, res.Run.ID, again.Run.ID)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs`).Scan(&n))
	assert.Equal(t, 1, n)

	// 解析失敗 → 檔案 failed 落地。
	_, err = svc.ImportSettlementFile(ctx, app.ImportCommand{Provider: "mock", Format: domain.FormatMockCSV, FileName: "bad.csv", Content: []byte("type,provider_reference,amount_minor,currency,fee_minor,settled_at\npayment,x,abc,TWD,0,2026-08-19T00:00:00Z\n"), SettlementDate: "2026-08-19"})
	require.ErrorIs(t, err, domain.ErrParse)
	var st string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM settlement_files WHERE file_name = 'bad.csv'`).Scan(&st))
	assert.Equal(t, "failed", st)

	// 消費事件 → 讀模型 → 下一次 run 少一筆 missing_in_ledger（mock_ch_…G5 補上）。
	ds, _, err := svc.ListDiscrepancies(ctx, app.DiscrepancyFilter{Kinds: []domain.DiscrepancyKind{domain.KindMissingInLedger}})
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, "mock_ch_01K2W3X4Y5Z6A7B8C9D0E1F2G5", ds[0].ProviderReference)

	resolved, err := svc.ResolveDiscrepancy(ctx, app.ResolveCommand{DiscrepancyID: ds[0].PublicID(), Action: app.ActionResolve, Note: "ok", ResolvedBy: "ops:it", IdempotencyKey: "k"})
	require.NoError(t, err)
	assert.Equal(t, domain.DiscrepancyResolved, resolved.Status)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type = 'reconciliation.discrepancy.resolved'`).Scan(&n))
	assert.Equal(t, 1, n)
}

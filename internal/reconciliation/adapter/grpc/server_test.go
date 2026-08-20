package grpc_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	reconciliationv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/reconciliation/v1"
	grpcadapter "github.com/tenghongzou/paymentgateway/internal/reconciliation/adapter/grpc"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app/porttest"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

var now = time.Date(2026, 8, 20, 6, 0, 0, 0, time.UTC)

func fixturePath() string {
	return filepath.Join("..", "..", "..", "..", "test", "fixtures", "settlement", "mock-2026-08-19.csv")
}

type env struct {
	client reconciliationv1.ReconciliationServiceClient
	store  *porttest.Store
}

func newEnv(t *testing.T, maxBytes int64) *env {
	t.Helper()
	store := porttest.NewStore()
	svc := app.NewService(store.Deps(porttest.NewClock(now)), app.Config{})
	srv := grpcadapter.NewServer(svc, grpcadapter.Options{MaxFileBytes: maxBytes})

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &env{client: reconciliationv1.NewReconciliationServiceClient(conn), store: store}
}

func importChunks(t *testing.T, c reconciliationv1.ReconciliationServiceClient, meta *reconciliationv1.ImportMetadata, content []byte, chunk int) (*reconciliationv1.ImportSettlementFileResponse, error) {
	t.Helper()
	stream, err := c.ImportSettlementFile(context.Background())
	require.NoError(t, err)
	if meta != nil {
		require.NoError(t, stream.Send(&reconciliationv1.ImportSettlementFileRequest{Payload: &reconciliationv1.ImportSettlementFileRequest_Metadata{Metadata: meta}}))
	}
	for i := 0; i < len(content); i += chunk {
		end := min(i+chunk, len(content))
		if err := stream.Send(&reconciliationv1.ImportSettlementFileRequest{Payload: &reconciliationv1.ImportSettlementFileRequest_Chunk{Chunk: content[i:end]}}); err != nil {
			break // server 可能已提早關閉（錯誤由 CloseAndRecv 回傳）
		}
	}
	return stream.CloseAndRecv()
}

func mockMeta() *reconciliationv1.ImportMetadata {
	return &reconciliationv1.ImportMetadata{
		Provider: "mock", FileFormat: reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_CSV,
		FileName: "mock-2026-08-19.csv", SettlementDate: "2026-08-19", TriggeredBy: "user:test",
	}
}

func TestImportSettlementFile_Chunks(t *testing.T) {
	e := newEnv(t, 0)
	content, err := os.ReadFile(fixturePath())
	require.NoError(t, err)
	u := ids.NewUUID()
	e.store.AddRecord(domain.PaymentRecord{
		ID: u, Kind: domain.RecordPayment, PublicID: ids.Format(ids.PrefixPayment, u), MerchantID: ids.NewUUID(), Provider: "mock",
		ProviderReference: "mock_ch_01K2W3X4Y5Z6A7B8C9D0E1F2G3", Amount: money.MustNew(1000, "TWD"), Status: domain.StatusCaptured,
		OccurredAt: now.Add(-5 * 24 * time.Hour), SourceSeq: 1,
	})

	resp, err := importChunks(t, e.client, mockMeta(), content, 64)
	require.NoError(t, err)
	assert.False(t, resp.GetAlreadyImported())
	run := resp.GetRun()
	require.NotNil(t, run)
	assert.True(t, ids.HasPrefix(run.GetId(), domain.PrefixRun))
	assert.Equal(t, reconciliationv1.RunStatus_RUN_STATUS_COMPLETED, run.GetStatus())
	assert.Equal(t, "2026-08-19", run.GetSettlementDate())
	assert.Equal(t, "mock-2026-08-19.csv", run.GetFileName())
	assert.Equal(t, domain.FileHash(content), run.GetFileChecksum())
	assert.Equal(t, int64(len(content)), run.GetFileSizeBytes())
	assert.Equal(t, reconciliationv1.SettlementFileFormat_SETTLEMENT_FILE_FORMAT_CSV, run.GetFileFormat())
	assert.Equal(t, "user:test", run.GetTriggeredBy())
	assert.Equal(t, int64(6), run.GetSummary().GetTotalRecords())
	assert.Equal(t, int64(1), run.GetSummary().GetMatched())
	assert.Equal(t, int64(4), run.GetSummary().GetDiscrepancies())
	assert.Equal(t, int64(4), run.GetSummary().GetDiscrepanciesByType()["DISCREPANCY_TYPE_MISSING_IN_LEDGER"])
	assert.NotNil(t, run.GetStartedAt())
	assert.NotNil(t, run.GetFinishedAt())
	var twdSettled int64
	for _, m := range run.GetSummary().GetTotalSettled() {
		if m.GetCurrency() == "TWD" {
			twdSettled = m.GetAmountMinor()
		}
	}
	assert.Equal(t, int64(251000), twdSettled)

	// 重複匯入 → already_imported。
	resp2, err := importChunks(t, e.client, mockMeta(), content, 1024)
	require.NoError(t, err)
	assert.True(t, resp2.GetAlreadyImported())
	assert.Equal(t, run.GetId(), resp2.GetRun().GetId())

	// Get。
	got, err := e.client.GetReconciliationRun(context.Background(), &reconciliationv1.GetReconciliationRunRequest{RunId: run.GetId()})
	require.NoError(t, err)
	assert.Equal(t, run.GetId(), got.GetRun().GetId())

	// List runs + filter。
	list, err := e.client.ListReconciliationRuns(context.Background(), &reconciliationv1.ListReconciliationRunsRequest{
		Provider: "mock", Statuses: []reconciliationv1.RunStatus{reconciliationv1.RunStatus_RUN_STATUS_COMPLETED},
		SettlementDateFrom: "2026-08-19", SettlementDateTo: "2026-08-19",
	})
	require.NoError(t, err)
	assert.Len(t, list.GetRuns(), 1)
	assert.False(t, list.GetPage().GetHasMore())
	list, err = e.client.ListReconciliationRuns(context.Background(), &reconciliationv1.ListReconciliationRunsRequest{SettlementDateFrom: "2026-08-20"})
	require.NoError(t, err)
	assert.Empty(t, list.GetRuns())

	// List discrepancies + mapping。
	ds, err := e.client.ListDiscrepancies(context.Background(), &reconciliationv1.ListDiscrepanciesRequest{RunId: run.GetId(), Page: &commonv1.PageRequest{PageSize: 2}})
	require.NoError(t, err)
	assert.Len(t, ds.GetDiscrepancies(), 2)
	assert.True(t, ds.GetPage().GetHasMore())
	ds2, err := e.client.ListDiscrepancies(context.Background(), &reconciliationv1.ListDiscrepanciesRequest{RunId: run.GetId(), Page: &commonv1.PageRequest{PageSize: 2, PageToken: ds.GetPage().GetNextPageToken()}})
	require.NoError(t, err)
	assert.Len(t, ds2.GetDiscrepancies(), 2)
	d := ds.GetDiscrepancies()[0]
	assert.True(t, ids.HasPrefix(d.GetId(), domain.PrefixDiscrepancy))
	assert.Equal(t, run.GetId(), d.GetRunId())
	assert.Equal(t, reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_MISSING_IN_LEDGER, d.GetType())
	assert.Equal(t, reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_OPEN, d.GetStatus())
	assert.NotEmpty(t, d.GetProviderReference())
	assert.NotEmpty(t, d.GetDescription())
	require.NotNil(t, d.GetSettlementRecord())
	assert.Equal(t, d.GetProviderReference(), d.GetSettlementRecord().GetProviderReference())
	assert.NotNil(t, d.GetActualAmount())
	assert.Nil(t, d.GetExpectedAmount())

	byType, err := e.client.ListDiscrepancies(context.Background(), &reconciliationv1.ListDiscrepanciesRequest{Types: []reconciliationv1.DiscrepancyType{reconciliationv1.DiscrepancyType_DISCREPANCY_TYPE_FEE_MISMATCH}})
	require.NoError(t, err)
	assert.Empty(t, byType.GetDiscrepancies())

	// Resolve。
	res, err := e.client.ResolveDiscrepancy(context.Background(), &reconciliationv1.ResolveDiscrepancyRequest{
		DiscrepancyId: d.GetId(), Action: reconciliationv1.ResolutionAction_RESOLUTION_ACTION_IGNORE, Note: "PSP T+2", ResolvedBy: "user:ops", IdempotencyKey: "idem-1",
	})
	require.NoError(t, err)
	assert.Equal(t, reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_IGNORED, res.GetDiscrepancy().GetStatus())
	assert.Equal(t, reconciliationv1.ResolutionAction_RESOLUTION_ACTION_IGNORE, res.GetDiscrepancy().GetResolutionAction())
	assert.Equal(t, "user:ops", res.GetDiscrepancy().GetResolvedBy())
	assert.NotNil(t, res.GetDiscrepancy().GetResolvedAt())

	// 狀態機錯誤 → FailedPrecondition + ErrorDetail。
	_, err = e.client.ResolveDiscrepancy(context.Background(), &reconciliationv1.ResolveDiscrepancyRequest{
		DiscrepancyId: d.GetId(), Action: reconciliationv1.ResolutionAction_RESOLUTION_ACTION_MARK_RESOLVED, ResolvedBy: "user:ops",
	})
	st, _ := status.FromError(err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Equal(t, "invalid_state_transition", grpcx.ErrorDetailFromStatus(st).GetCode())

	// 只篩 resolved/ignored。
	closed, err := e.client.ListDiscrepancies(context.Background(), &reconciliationv1.ListDiscrepanciesRequest{Statuses: []reconciliationv1.DiscrepancyStatus{reconciliationv1.DiscrepancyStatus_DISCREPANCY_STATUS_IGNORED}})
	require.NoError(t, err)
	assert.Len(t, closed.GetDiscrepancies(), 1)
}

func TestImportSettlementFile_Errors(t *testing.T) {
	e := newEnv(t, 200)
	content, err := os.ReadFile(fixturePath())
	require.NoError(t, err)

	code := func(err error) codes.Code { st, _ := status.FromError(err); return st.Code() }

	t.Run("no metadata", func(t *testing.T) {
		_, err := importChunks(t, e.client, nil, content, 64)
		assert.Equal(t, codes.InvalidArgument, code(err))
	})
	t.Run("too large", func(t *testing.T) {
		_, err := importChunks(t, e.client, mockMeta(), content, 64)
		assert.Equal(t, codes.ResourceExhausted, code(err))
	})
	t.Run("empty content", func(t *testing.T) {
		_, err := importChunks(t, e.client, mockMeta(), nil, 64)
		assert.Equal(t, codes.InvalidArgument, code(err))
	})
	t.Run("unknown provider", func(t *testing.T) {
		m := mockMeta()
		m.Provider = "adyen"
		_, err := importChunks(t, e.client, m, []byte("x"), 64)
		assert.Equal(t, codes.InvalidArgument, code(err))
	})
	t.Run("missing provider", func(t *testing.T) {
		m := mockMeta()
		m.Provider = ""
		_, err := importChunks(t, e.client, m, []byte("x"), 64)
		assert.Equal(t, codes.InvalidArgument, code(err))
	})
	t.Run("parse error", func(t *testing.T) {
		_, err := importChunks(t, e.client, mockMeta(), []byte("type,provider_reference,amount_minor,currency,fee_minor,settled_at\npayment,x,abc,TWD,0,2026-08-19T00:00:00Z\n"), 64)
		require.Equal(t, codes.InvalidArgument, code(err))
		st, _ := status.FromError(err)
		assert.Contains(t, st.Message(), "amount_minor")
	})
	t.Run("bad settlement date", func(t *testing.T) {
		m := mockMeta()
		m.SettlementDate = "bad"
		_, err := importChunks(t, e.client, m, []byte("x"), 64)
		assert.Equal(t, codes.InvalidArgument, code(err))
	})
	t.Run("checksum mismatch", func(t *testing.T) {
		m := mockMeta()
		m.ExpectedChecksum = "00"
		_, err := importChunks(t, e.client, m, content[:150], 64)
		assert.Equal(t, codes.InvalidArgument, code(err))
	})
	t.Run("get unknown run", func(t *testing.T) {
		_, err := e.client.GetReconciliationRun(context.Background(), &reconciliationv1.GetReconciliationRunRequest{RunId: ids.New(domain.PrefixRun)})
		assert.Equal(t, codes.NotFound, code(err))
		_, err = e.client.GetReconciliationRun(context.Background(), &reconciliationv1.GetReconciliationRunRequest{})
		assert.Equal(t, codes.InvalidArgument, code(err))
	})
	t.Run("resolve validation", func(t *testing.T) {
		_, err := e.client.ResolveDiscrepancy(context.Background(), &reconciliationv1.ResolveDiscrepancyRequest{DiscrepancyId: ids.New(domain.PrefixDiscrepancy), Action: reconciliationv1.ResolutionAction_RESOLUTION_ACTION_ADJUST_LEDGER, ResolvedBy: "x"})
		assert.Equal(t, codes.Unimplemented, code(err))
		_, err = e.client.ResolveDiscrepancy(context.Background(), &reconciliationv1.ResolveDiscrepancyRequest{DiscrepancyId: ids.New(domain.PrefixDiscrepancy), ResolvedBy: "x"})
		assert.Equal(t, codes.InvalidArgument, code(err))
		_, err = e.client.ResolveDiscrepancy(context.Background(), &reconciliationv1.ResolveDiscrepancyRequest{DiscrepancyId: ids.New(domain.PrefixDiscrepancy), Action: reconciliationv1.ResolutionAction_RESOLUTION_ACTION_MARK_RESOLVED, ResolvedBy: "x"})
		assert.Equal(t, codes.NotFound, code(err))
		_, err = e.client.ResolveDiscrepancy(context.Background(), &reconciliationv1.ResolveDiscrepancyRequest{Action: reconciliationv1.ResolutionAction_RESOLUTION_ACTION_MARK_RESOLVED, ResolvedBy: "x"})
		assert.Equal(t, codes.InvalidArgument, code(err))
	})
	t.Run("list discrepancies bad run id", func(t *testing.T) {
		_, err := e.client.ListDiscrepancies(context.Background(), &reconciliationv1.ListDiscrepanciesRequest{RunId: "nope"})
		assert.Equal(t, codes.NotFound, code(err))
		_, err = e.client.ListDiscrepancies(context.Background(), &reconciliationv1.ListDiscrepanciesRequest{MerchantId: "nope"})
		assert.Equal(t, codes.InvalidArgument, code(err))
		_, err = e.client.ListDiscrepancies(context.Background(), &reconciliationv1.ListDiscrepanciesRequest{MerchantId: ids.New(ids.PrefixMerchant)})
		assert.NoError(t, err)
	})
	t.Run("list runs bad date", func(t *testing.T) {
		_, err := e.client.ListReconciliationRuns(context.Background(), &reconciliationv1.ListReconciliationRunsRequest{SettlementDateFrom: "x"})
		assert.Equal(t, codes.InvalidArgument, code(err))
	})
}

func TestImportSettlementFile_SourceURL(t *testing.T) {
	e := newEnv(t, 0)
	abs, err := filepath.Abs(fixturePath())
	require.NoError(t, err)

	t.Run("file url", func(t *testing.T) {
		m := mockMeta()
		m.SourceUrl = "file://" + filepath.ToSlash(abs)
		resp, err := importChunks(t, e.client, m, nil, 64)
		require.NoError(t, err)
		assert.Equal(t, reconciliationv1.RunStatus_RUN_STATUS_COMPLETED, resp.GetRun().GetStatus())
		assert.Equal(t, int64(6), resp.GetRun().GetSummary().GetTotalRecords())
	})
	t.Run("http url", func(t *testing.T) {
		content, _ := os.ReadFile(abs)
		body := append([]byte(nil), content...)
		body = append(body, []byte("payment,mock_ch_HTTP,1,TWD,0,2026-08-19T00:00:00Z\n")...)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
		defer ts.Close()
		m := mockMeta()
		m.SourceUrl = ts.URL + "/mock.csv"
		resp, err := importChunks(t, e.client, m, nil, 64)
		require.NoError(t, err)
		assert.Equal(t, int64(7), resp.GetRun().GetSummary().GetTotalRecords())
	})
	t.Run("http error status", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
		defer ts.Close()
		m := mockMeta()
		m.SourceUrl = ts.URL
		_, err := importChunks(t, e.client, m, nil, 64)
		st, _ := status.FromError(err)
		assert.Equal(t, codes.InvalidArgument, st.Code())
	})
	t.Run("unsupported scheme", func(t *testing.T) {
		m := mockMeta()
		m.SourceUrl = "s3://bucket/key.csv"
		_, err := importChunks(t, e.client, m, nil, 64)
		st, _ := status.FromError(err)
		assert.Equal(t, codes.InvalidArgument, st.Code())
	})
	t.Run("missing file", func(t *testing.T) {
		m := mockMeta()
		m.SourceUrl = "file:///definitely/not/here.csv"
		_, err := importChunks(t, e.client, m, nil, 64)
		st, _ := status.FromError(err)
		assert.Equal(t, codes.NotFound, st.Code())
	})
}

func TestFetcher_Limits(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.csv")
	require.NoError(t, os.WriteFile(big, make([]byte, 2048), 0o600))
	small := filepath.Join(dir, "small.csv")
	require.NoError(t, os.WriteFile(small, []byte("x"), 0o600))

	f := grpcadapter.NewFetcher(grpcadapter.FetcherOptions{MaxBytes: 1024, AllowedDirs: []string{dir}})
	_, err := f.Fetch(context.Background(), "file://"+filepath.ToSlash(big))
	assert.ErrorIs(t, err, grpcadapter.ErrFileTooLarge)
	b, err := f.Fetch(context.Background(), "file://"+filepath.ToSlash(small))
	require.NoError(t, err)
	assert.Equal(t, []byte("x"), b)

	abs, _ := filepath.Abs(fixturePath())
	_, err = f.Fetch(context.Background(), "file://"+filepath.ToSlash(abs))
	require.Error(t, err, "AllowedDirs 之外")
	st := grpcx.ErrorFromDomain(err)
	assert.Equal(t, codes.PermissionDenied, status.Code(st))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(make([]byte, 4096)) }))
	defer ts.Close()
	_, err = f.Fetch(context.Background(), ts.URL)
	assert.ErrorIs(t, err, grpcadapter.ErrFileTooLarge)
	assert.Equal(t, codes.ResourceExhausted, status.Code(grpcx.ErrorFromDomain(err)))
}

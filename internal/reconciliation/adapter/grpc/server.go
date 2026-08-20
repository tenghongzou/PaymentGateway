// Package grpc 為 reconciliation-service 的 gRPC adapter（pg.reconciliation.v1.ReconciliationService）。
package grpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	reconciliationv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/reconciliation/v1"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// DefaultMaxFileBytes 為 Phase 0 的結算檔大小上限（50MB；proto 註解的 512MB 待物件儲存與串流解析完成後再放寬）。
const DefaultMaxFileBytes = 50 << 20

// UseCases 為 Server 需要的 use case 介面（由 *app.Service 實作）。
type UseCases interface {
	ImportSettlementFile(ctx context.Context, cmd app.ImportCommand) (*app.ImportResult, error)
	GetReconciliationRun(ctx context.Context, publicID string) (*domain.Run, error)
	ListReconciliationRuns(ctx context.Context, f app.RunFilter) ([]domain.Run, string, error)
	ListDiscrepancies(ctx context.Context, f app.DiscrepancyFilter) ([]domain.Discrepancy, string, error)
	ResolveDiscrepancy(ctx context.Context, cmd app.ResolveCommand) (*domain.Discrepancy, error)
}

// Options 為 Server 選項。
type Options struct {
	// MaxFileBytes 為接受的結算檔上限（chunk 累計或 URL 下載）；0 → DefaultMaxFileBytes。
	MaxFileBytes int64
	// Fetcher 負責 source_url 下載；nil → NewFetcher 預設（file:// 與 http(s)://）。
	Fetcher Fetcher
	Logger  *slog.Logger
}

// Server 實作 pg.reconciliation.v1.ReconciliationService。
type Server struct {
	reconciliationv1.UnimplementedReconciliationServiceServer
	uc      UseCases
	max     int64
	fetcher Fetcher
	log     *slog.Logger
}

// NewServer 建立 Server。
func NewServer(uc UseCases, opts Options) *Server {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Fetcher == nil {
		opts.Fetcher = NewFetcher(FetcherOptions{MaxBytes: opts.MaxFileBytes})
	}
	return &Server{uc: uc, max: opts.MaxFileBytes, fetcher: opts.Fetcher, log: opts.Logger}
}

// Register 把服務註冊到 gRPC server。
func (s *Server) Register(srv *grpc.Server) {
	reconciliationv1.RegisterReconciliationServiceServer(srv, s)
}

// ImportSettlementFile 實作 client streaming：第一則必須是 metadata；有 source_url 則服務端下載，否則累積 chunk。
func (s *Server) ImportSettlementFile(stream grpc.ClientStreamingServer[reconciliationv1.ImportSettlementFileRequest, reconciliationv1.ImportSettlementFileResponse]) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "first message must be metadata")
		}
		return err
	}
	meta := first.GetMetadata()
	if meta == nil {
		return status.Error(codes.InvalidArgument, "first message must be metadata")
	}
	format, err := formatFor(meta.GetProvider(), meta.GetFileFormat())
	if err != nil {
		return grpcx.ErrorFromDomain(err)
	}

	var content []byte
	if url := strings.TrimSpace(meta.GetSourceUrl()); url != "" {
		content, err = s.fetcher.Fetch(ctx, url)
		if err != nil {
			return grpcx.ErrorFromDomain(err)
		}
		// 清空剩餘訊息（client 不應再送 chunk，但容忍）。
		for {
			if _, err := stream.Recv(); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
		}
	} else {
		content, err = s.readChunks(stream)
		if err != nil {
			return err
		}
	}

	res, err := s.uc.ImportSettlementFile(ctx, app.ImportCommand{
		Provider:         meta.GetProvider(),
		Format:           format,
		FileName:         meta.GetFileName(),
		Content:          content,
		SettlementDate:   meta.GetSettlementDate(),
		TriggeredBy:      meta.GetTriggeredBy(),
		StorageURI:       meta.GetSourceUrl(),
		ExpectedChecksum: meta.GetExpectedChecksum(),
	})
	if err != nil {
		return grpcx.ErrorFromDomain(err)
	}
	return stream.SendAndClose(&reconciliationv1.ImportSettlementFileResponse{
		Run:             runToProto(res.Run),
		AlreadyImported: res.AlreadyImported,
	})
}

// readChunks 累積 chunk 直到 EOF，超過上限回 RESOURCE_EXHAUSTED。
func (s *Server) readChunks(stream grpc.ClientStreamingServer[reconciliationv1.ImportSettlementFileRequest, reconciliationv1.ImportSettlementFileResponse]) ([]byte, error) {
	var buf []byte
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		chunk := msg.GetChunk()
		if msg.GetMetadata() != nil {
			return nil, status.Error(codes.InvalidArgument, "metadata must only appear in the first message")
		}
		if int64(len(buf)+len(chunk)) > s.max {
			return nil, status.Errorf(codes.ResourceExhausted, "settlement file exceeds %d bytes", s.max)
		}
		buf = append(buf, chunk...)
	}
	if len(buf) == 0 {
		return nil, status.Error(codes.InvalidArgument, "settlement file content is empty (send chunks or source_url)")
	}
	return buf, nil
}

// GetReconciliationRun 實作 rpc。
func (s *Server) GetReconciliationRun(ctx context.Context, req *reconciliationv1.GetReconciliationRunRequest) (*reconciliationv1.GetReconciliationRunResponse, error) {
	if req.GetRunId() == "" {
		return nil, grpcx.ErrorFromDomain(apperr.ErrParameterMissing.WithMessage("run_id is required.").WithParam("run_id"))
	}
	run, err := s.uc.GetReconciliationRun(ctx, req.GetRunId())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &reconciliationv1.GetReconciliationRunResponse{Run: runToProto(run)}, nil
}

// ListReconciliationRuns 實作 rpc。
func (s *Server) ListReconciliationRuns(ctx context.Context, req *reconciliationv1.ListReconciliationRunsRequest) (*reconciliationv1.ListReconciliationRunsResponse, error) {
	f := app.RunFilter{
		Provider:  req.GetProvider(),
		PageSize:  int(req.GetPage().GetPageSize()),
		PageToken: req.GetPage().GetPageToken(),
	}
	for _, st := range req.GetStatuses() {
		if ds, ok := runStatusFromProto(st); ok {
			f.Statuses = append(f.Statuses, ds)
		}
	}
	var err error
	if f.DateFrom, err = parseDate(req.GetSettlementDateFrom(), "settlement_date_from"); err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	if f.DateTo, err = parseDate(req.GetSettlementDateTo(), "settlement_date_to"); err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	runs, next, err := s.uc.ListReconciliationRuns(ctx, f)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	out := make([]*reconciliationv1.ReconciliationRun, 0, len(runs))
	for i := range runs {
		out = append(out, runToProto(&runs[i]))
	}
	return &reconciliationv1.ListReconciliationRunsResponse{Runs: out, Page: pageResponse(next)}, nil
}

// ListDiscrepancies 實作 rpc。
func (s *Server) ListDiscrepancies(ctx context.Context, req *reconciliationv1.ListDiscrepanciesRequest) (*reconciliationv1.ListDiscrepanciesResponse, error) {
	f := app.DiscrepancyFilter{
		Provider:  req.GetProvider(),
		PaymentID: req.GetPaymentId(),
		PageSize:  int(req.GetPage().GetPageSize()),
		PageToken: req.GetPage().GetPageToken(),
	}
	if rid := req.GetRunId(); rid != "" {
		id, err := domain.ParseRunID(rid)
		if err != nil {
			return nil, grpcx.ErrorFromDomain(err)
		}
		f.RunID = &id
	}
	if mid := req.GetMerchantId(); mid != "" {
		id, err := parseMerchantID(mid)
		if err != nil {
			return nil, grpcx.ErrorFromDomain(err)
		}
		f.MerchantID = &id
	}
	for _, t := range req.GetTypes() {
		k, err := kindFromProto(t)
		if err != nil {
			return nil, grpcx.ErrorFromDomain(err)
		}
		f.Kinds = append(f.Kinds, k...)
	}
	for _, st := range req.GetStatuses() {
		if ds, ok := discrepancyStatusFromProto(st); ok {
			f.Statuses = append(f.Statuses, ds)
		}
	}
	if ts := req.GetCreatedAfter(); ts != nil {
		f.CreatedAfter = ts.AsTime()
	}
	if ts := req.GetCreatedBefore(); ts != nil {
		f.CreatedBefore = ts.AsTime()
	}
	ds, next, err := s.uc.ListDiscrepancies(ctx, f)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	out := make([]*reconciliationv1.Discrepancy, 0, len(ds))
	for i := range ds {
		out = append(out, discrepancyToProto(&ds[i]))
	}
	return &reconciliationv1.ListDiscrepanciesResponse{Discrepancies: out, Page: pageResponse(next)}, nil
}

// ResolveDiscrepancy 實作 rpc（Phase 0：MARK_RESOLVED / IGNORE；ADJUST_LEDGER / RESYNC_PAYMENT 回 Unimplemented）。
func (s *Server) ResolveDiscrepancy(ctx context.Context, req *reconciliationv1.ResolveDiscrepancyRequest) (*reconciliationv1.ResolveDiscrepancyResponse, error) {
	if req.GetDiscrepancyId() == "" {
		return nil, grpcx.ErrorFromDomain(apperr.ErrParameterMissing.WithMessage("discrepancy_id is required.").WithParam("discrepancy_id"))
	}
	var action app.ResolutionAction
	switch req.GetAction() {
	case reconciliationv1.ResolutionAction_RESOLUTION_ACTION_MARK_RESOLVED:
		action = app.ActionResolve
	case reconciliationv1.ResolutionAction_RESOLUTION_ACTION_IGNORE:
		action = app.ActionIgnore
	case reconciliationv1.ResolutionAction_RESOLUTION_ACTION_ADJUST_LEDGER, reconciliationv1.ResolutionAction_RESOLUTION_ACTION_RESYNC_PAYMENT:
		// TODO(phase-1)：ADJUST_LEDGER 改由 ledger 消費 discrepancy 事件 + ops 工具 J-REV（docs/05 §9.2 第 6 點）；RESYNC_PAYMENT 需 payment-service GetPayment。
		return nil, status.Errorf(codes.Unimplemented, "resolution action %s is not supported yet", req.GetAction())
	default:
		return nil, grpcx.ErrorFromDomain(apperr.ErrParameterMissing.WithMessage("action is required.").WithParam("action"))
	}
	d, err := s.uc.ResolveDiscrepancy(ctx, app.ResolveCommand{
		DiscrepancyID:  req.GetDiscrepancyId(),
		Action:         action,
		Note:           req.GetNote(),
		ResolvedBy:     req.GetResolvedBy(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &reconciliationv1.ResolveDiscrepancyResponse{Discrepancy: discrepancyToProto(d)}, nil
}

// parseDate 解析 YYYY-MM-DD（空字串 → 零值）。
func parseDate(s, param string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, apperr.ErrParameterInvalid.WithMessage("%s must be YYYY-MM-DD.", param).WithParam(param)
	}
	return t.UTC(), nil
}

// parseMerchantID 接受 mch_… 或裸 uuid。
func parseMerchantID(s string) (uuid.UUID, error) {
	if u, err := uuid.Parse(s); err == nil {
		return u, nil
	}
	if strings.HasPrefix(s, "mch_") {
		if _, u, err := ids.Parse(s); err == nil {
			return u, nil
		}
	}
	return uuid.Nil, apperr.ErrParameterInvalid.WithMessage("merchant_id must be mch_… or uuid.").WithParam("merchant_id")
}

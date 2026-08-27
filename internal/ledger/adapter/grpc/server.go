// Package grpc 為 ledger-service 的 gRPC adapter：實作 pg.ledger.v1.LedgerService 的 7 個 RPC，
// 把 proto 轉成 app 的輸入、把領域錯誤轉成 gRPC status（pkg/grpcx.ErrorFromDomain）。
package grpc

import (
	"context"
	"strconv"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	ledgerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/ledger/v1"
	"github.com/tenghongzou/paymentgateway/internal/ledger/app"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// Server 實作 pg.ledger.v1.LedgerService。
type Server struct {
	ledgerv1.UnimplementedLedgerServiceServer
	svc *app.Service
}

// NewServer 建立 Server。
func NewServer(svc *app.Service) *Server { return &Server{svc: svc} }

// Register 把服務註冊到 gRPC server。
func (s *Server) Register(srv *grpc.Server) {
	ledgerv1.RegisterLedgerServiceServer(srv, s)
}

// accountKeyFromRequest 把 (type, merchant_id, provider, currency, livemode) 組成 AccountKey。
func accountKeyFromRequest(typ ledgerv1.AccountType, merchantID, provider, currency string, livemode bool) (domain.AccountKey, error) {
	kind, err := app.KindFromProto(typ)
	if err != nil {
		return domain.AccountKey{}, err
	}
	code, err := domain.CodeFor(kind, provider)
	if err != nil {
		return domain.AccountKey{}, err
	}
	key := domain.AccountKey{Code: code, Currency: currency, Livemode: livemode}
	if merchantID != "" {
		m, err := app.ParseMerchantID(merchantID)
		if err != nil {
			return domain.AccountKey{}, err
		}
		key.MerchantID = m
	}
	if currency == "" {
		return domain.AccountKey{}, apperr.ErrParameterMissing.WithParam("currency").WithMessage("currency is required")
	}
	return key, key.Validate()
}

// CreateAccount 建立帳戶（冪等：已存在回傳既有帳戶）。
func (s *Server) CreateAccount(ctx context.Context, req *ledgerv1.CreateAccountRequest) (*ledgerv1.CreateAccountResponse, error) {
	key, err := accountKeyFromRequest(req.GetType(), req.GetMerchantId(), req.GetProvider(), req.GetCurrency(), req.GetLivemode())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	acct, existed, err := s.svc.CreateAccount(ctx, key)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &ledgerv1.CreateAccountResponse{Account: app.ToProtoAccount(acct), AlreadyExisted: existed}, nil
}

// GetAccount 依 ID 取得帳戶。
func (s *Server) GetAccount(ctx context.Context, req *ledgerv1.GetAccountRequest) (*ledgerv1.GetAccountResponse, error) {
	id, err := app.ParseAccountID(req.GetAccountId())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	acct, err := s.svc.GetAccount(ctx, id)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &ledgerv1.GetAccountResponse{Account: app.ToProtoAccount(acct)}, nil
}

// ListAccounts 列出帳戶。
func (s *Server) ListAccounts(ctx context.Context, req *ledgerv1.ListAccountsRequest) (*ledgerv1.ListAccountsResponse, error) {
	page, err := app.NewPage(int(req.GetPage().GetPageSize()), req.GetPage().GetPageToken())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	f := app.AccountFilter{Currency: req.GetCurrency(), Qualifier: req.GetProvider()}
	if req.GetMerchantId() != "" {
		m, merr := app.ParseMerchantID(req.GetMerchantId())
		if merr != nil {
			return nil, grpcx.ErrorFromDomain(merr)
		}
		f.MerchantID = &m
	}
	if req.GetType() != ledgerv1.AccountType_ACCOUNT_TYPE_UNSPECIFIED {
		kind, kerr := app.KindFromProto(req.GetType())
		if kerr != nil {
			return nil, grpcx.ErrorFromDomain(kerr)
		}
		f.Kind = kind
	}
	if req.Livemode != nil {
		live := req.GetLivemode()
		f.Livemode = &live
	}
	accts, next, err := s.svc.ListAccounts(ctx, f, page)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	resp := &ledgerv1.ListAccountsResponse{Page: pageResponse(next)}
	for _, a := range accts {
		resp.Accounts = append(resp.Accounts, app.ToProtoAccount(a))
	}
	return resp, nil
}

// GetBalance 取得帳戶餘額或商戶餘額拆解。
func (s *Server) GetBalance(ctx context.Context, req *ledgerv1.GetBalanceRequest) (*ledgerv1.GetBalanceResponse, error) {
	switch t := req.GetTarget().(type) {
	case *ledgerv1.GetBalanceRequest_AccountId:
		id, err := app.ParseAccountID(t.AccountId)
		if err != nil {
			return nil, grpcx.ErrorFromDomain(err)
		}
		b, err := s.svc.GetBalance(ctx, id)
		if err != nil {
			return nil, grpcx.ErrorFromDomain(err)
		}
		return &ledgerv1.GetBalanceResponse{Balance: app.ToProtoBalance(b)}, nil
	case *ledgerv1.GetBalanceRequest_Merchant:
		m, err := app.ParseMerchantID(t.Merchant.GetMerchantId())
		if err != nil {
			return nil, grpcx.ErrorFromDomain(err)
		}
		mbs, err := s.svc.GetMerchantBalances(ctx, m, t.Merchant.GetCurrency(), t.Merchant.GetLivemode())
		if err != nil {
			return nil, grpcx.ErrorFromDomain(err)
		}
		resp := &ledgerv1.GetBalanceResponse{}
		for _, mb := range mbs {
			resp.MerchantBalances = append(resp.MerchantBalances, app.ToProtoMerchantBalance(mb))
		}
		return resp, nil
	default:
		return nil, grpcx.ErrorFromDomain(apperr.ErrParameterMissing.WithParam("target").WithMessage("account_id or merchant is required"))
	}
}

// PostJournal 內部過帳（調整 / 沖銷）。
func (s *Server) PostJournal(ctx context.Context, req *ledgerv1.PostJournalRequest) (*ledgerv1.PostJournalResponse, error) {
	srcType, err := app.SourceTypeFromProto(req.GetSourceType())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	in := app.PostJournalInput{
		IdempotencyKey: req.GetIdempotencyKey(),
		SourceType:     srcType,
		SourceID:       req.GetSourceId(),
		ReferenceType:  domain.ReferenceType(req.GetReferenceType()),
		ReferenceID:    req.GetReferenceId(),
		Description:    req.GetDescription(),
		Livemode:       req.GetLivemode(),
		Metadata:       req.GetMetadata(),
	}
	if req.GetMerchantId() != "" {
		m, merr := app.ParseMerchantID(req.GetMerchantId())
		if merr != nil {
			return nil, grpcx.ErrorFromDomain(merr)
		}
		in.MerchantID = m
	}
	if req.GetEffectiveAt() != nil {
		in.EffectiveAt = req.GetEffectiveAt().AsTime()
	}
	if req.GetReversesJournalId() != "" {
		id, jerr := app.ParseJournalID(req.GetReversesJournalId())
		if jerr != nil {
			return nil, grpcx.ErrorFromDomain(jerr)
		}
		in.ReversesJournalID = &id
	}
	for i, e := range req.GetEntries() {
		acctID, eerr := app.ParseAccountID(e.GetAccountId())
		if eerr != nil {
			return nil, grpcx.ErrorFromDomain(apperr.From(eerr).WithParam(entryParam(i, "account_id")))
		}
		dir, eerr := app.DirectionFromProto(e.GetDirection())
		if eerr != nil {
			return nil, grpcx.ErrorFromDomain(apperr.From(eerr).WithParam(entryParam(i, "direction")))
		}
		amt, eerr := money.FromProto(e.GetAmount())
		if eerr != nil {
			return nil, grpcx.ErrorFromDomain(domain.ErrInvalidCurrency.WithParam(entryParam(i, "amount")).Wrap(eerr))
		}
		in.Entries = append(in.Entries, app.EntryInput{AccountID: acctID, Direction: dir, Amount: amt, Description: e.GetDescription()})
	}
	j, replayed, err := s.svc.PostManualJournal(ctx, in)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &ledgerv1.PostJournalResponse{Journal: app.ToProtoJournal(j), IdempotentReplayed: replayed}, nil
}

// GetJournal 依 ID 取得 journal。
func (s *Server) GetJournal(ctx context.Context, req *ledgerv1.GetJournalRequest) (*ledgerv1.GetJournalResponse, error) {
	id, err := app.ParseJournalID(req.GetJournalId())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	merchant := uuid.Nil
	if req.GetMerchantId() != "" {
		if merchant, err = app.ParseMerchantID(req.GetMerchantId()); err != nil {
			return nil, grpcx.ErrorFromDomain(err)
		}
	}
	j, err := s.svc.GetJournal(ctx, id, merchant)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &ledgerv1.GetJournalResponse{Journal: app.ToProtoJournal(j)}, nil
}

// ListJournals 列出 journal。
func (s *Server) ListJournals(ctx context.Context, req *ledgerv1.ListJournalsRequest) (*ledgerv1.ListJournalsResponse, error) {
	page, err := app.NewPage(int(req.GetPage().GetPageSize()), req.GetPage().GetPageToken())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	f := app.JournalFilter{
		ReferenceType: domain.ReferenceType(req.GetReferenceType()),
		ReferenceID:   req.GetReferenceId(),
		Currency:      req.GetCurrency(),
	}
	if req.GetMerchantId() != "" {
		m, merr := app.ParseMerchantID(req.GetMerchantId())
		if merr != nil {
			return nil, grpcx.ErrorFromDomain(merr)
		}
		f.MerchantID = &m
	}
	if req.GetAccountId() != "" {
		a, aerr := app.ParseAccountID(req.GetAccountId())
		if aerr != nil {
			return nil, grpcx.ErrorFromDomain(aerr)
		}
		f.AccountID = &a
	}
	if req.GetSourceType() != ledgerv1.JournalSourceType_JOURNAL_SOURCE_TYPE_UNSPECIFIED {
		st, serr := app.SourceTypeFromProto(req.GetSourceType())
		if serr != nil {
			return nil, grpcx.ErrorFromDomain(serr)
		}
		f.SourceType = st
	}
	if req.GetPostedAfter() != nil {
		t := req.GetPostedAfter().AsTime()
		f.PostedAfter = &t
	}
	if req.GetPostedBefore() != nil {
		t := req.GetPostedBefore().AsTime()
		f.PostedBefore = &t
	}
	if req.Livemode != nil {
		live := req.GetLivemode()
		f.Livemode = &live
	}
	js, next, err := s.svc.ListJournals(ctx, f, page)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	resp := &ledgerv1.ListJournalsResponse{Page: pageResponse(next)}
	for _, j := range js {
		resp.Journals = append(resp.Journals, app.ToProtoJournal(j))
	}
	return resp, nil
}

func pageResponse(next *app.Cursor) *commonv1.PageResponse {
	token := app.EncodeCursor(next)
	return &commonv1.PageResponse{NextPageToken: token, HasMore: token != ""}
}

func entryParam(i int, field string) string {
	return "entries[" + strconv.Itoa(i) + "]." + field
}

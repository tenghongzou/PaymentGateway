package app

import (
	"context"
	"errors"

	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/money"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// GetPayment 依公開 ID 取得付款（跨商戶查詢回 resource_missing）。
func (s *Service) GetPayment(ctx context.Context, merchantID, paymentID string) (*domain.Payment, error) {
	return s.repo.GetPayment(ctx, merchantID, paymentID)
}

// ListPayments 列出付款。
func (s *Service) ListPayments(ctx context.Context, merchantID string, f ListFilter) ([]*domain.Payment, string, error) {
	if f.Limit <= 0 {
		f.Limit = 20
	}
	if f.Limit > 100 {
		f.Limit = 100
	}
	return s.repo.ListPayments(ctx, merchantID, f)
}

// CaptureCommand 為請款輸入。
type CaptureCommand struct {
	MerchantID     string
	PaymentID      string
	IdempotencyKey string
	// Amount 為 nil 時請款全部剩餘授權額度。
	Amount *money.Money
	Final  bool
}

// CapturePayment 兩階段請款（docs/02 §8.3）：tx1 驗證 → adapter Capture（無 DB 鎖）→ tx2 套用結果。
// PSP 拒絕時回 provider_error，Payment 維持 authorized（不自動 void）。
func (s *Service) CapturePayment(ctx context.Context, cmd CaptureCommand) (*domain.Payment, error) {
	p, err := s.repo.GetPayment(ctx, cmd.MerchantID, cmd.PaymentID)
	if err != nil {
		return nil, err
	}
	if p.Status == domain.StatusCaptured && cmd.IdempotencyKey != "" {
		// v1 單次 capture；已 captured 的重複請求回目前狀態（冪等）。
		return p, nil
	}
	amount := p.RemainingAuthorized()
	if cmd.Amount != nil {
		amount = *cmd.Amount
	}
	// 先以複本驗證守衛（狀態、金額、幣別、授權期限），避免呼叫 PSP 後才發現不合法。
	probe := *p
	if _, perr := probe.Capture(amount, 0, s.clock.Now()); perr != nil {
		return nil, perr
	}
	loadedVersion := p.Version

	client, ok := s.providerFor(p.SelectedProvider)
	if !ok {
		return nil, domain.ErrProviderUnavailable
	}
	cctx, cancel := s.callCtx(ctx)
	resp, perr := client.Capture(cctx, &providerv1.CaptureRequest{
		PaymentId: p.PublicID, ProviderReference: p.ProviderReference, IdempotencyKey: p.PublicID + ":capture:1",
		Amount: amount.ToProto(), Final: cmd.Final, TestMode: !p.LiveMode,
	})
	cancel()
	succeeded, cat, code, msg := resultCategory(resp.GetResult(), perr)
	if !succeeded {
		if cat == domain.CategoryProviderTimeout || cat == domain.CategoryUnknown {
			// TODO(operation-reconciler)：以 GetPaymentStatus 收斂 capture 結果；Phase 0 直接回 504。
			return nil, domain.ErrProviderTimeout
		}
		return nil, domain.ProviderError(cat, code, msg)
	}
	fee := resp.GetFee().GetAmountMinor()
	if m, merr := money.FromProto(resp.GetCapturedAmount()); merr == nil && m.Currency == amount.Currency && m.IsPositive() {
		amount = m
	}

	err = s.tx.WithinTx(context.WithoutCancel(ctx), func(ctx context.Context) error {
		cur, gerr := s.repo.GetPaymentForUpdate(ctx, cmd.MerchantID, cmd.PaymentID)
		if gerr != nil {
			return gerr
		}
		if cur.Version != loadedVersion {
			return pgdb.ErrConcurrentModification
		}
		ev, cerr := cur.Capture(amount, fee, s.clock.Now())
		if cerr != nil {
			return cerr
		}
		p = cur
		return s.persist(ctx, cur, loadedVersion, nil, []domain.Event{ev})
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// VoidCommand 為取消授權輸入。
type VoidCommand struct {
	MerchantID     string
	PaymentID      string
	IdempotencyKey string
	Reason         string // requested_by_customer | duplicate | fraudulent | abandoned（寫入事件 payload）
}

// VoidPayment 取消授權（requires_action / authorized → voided）。
func (s *Service) VoidPayment(ctx context.Context, cmd VoidCommand) (*domain.Payment, error) {
	p, err := s.repo.GetPayment(ctx, cmd.MerchantID, cmd.PaymentID)
	if err != nil {
		return nil, err
	}
	if p.Status == domain.StatusVoided {
		return p, nil
	}
	probe := *p
	if _, verr := probe.Void(domain.VoidReasonMerchantRequest, cmd.Reason, s.clock.Now()); verr != nil {
		return nil, verr
	}
	loadedVersion := p.Version

	if p.ProviderReference != "" {
		client, ok := s.providerFor(p.SelectedProvider)
		if !ok {
			return nil, domain.ErrProviderUnavailable
		}
		cctx, cancel := s.callCtx(ctx)
		resp, perr := client.Void(cctx, &providerv1.VoidRequest{PaymentId: p.PublicID, ProviderReference: p.ProviderReference, IdempotencyKey: p.PublicID + ":void", Reason: cmd.Reason, TestMode: !p.LiveMode})
		cancel()
		if ok, cat, code, msg := resultCategory(resp.GetResult(), perr); !ok {
			if cat == domain.CategoryProviderTimeout || cat == domain.CategoryUnknown {
				return nil, domain.ErrProviderTimeout
			}
			return nil, domain.ProviderError(cat, code, msg)
		}
	}

	err = s.tx.WithinTx(context.WithoutCancel(ctx), func(ctx context.Context) error {
		cur, gerr := s.repo.GetPaymentForUpdate(ctx, cmd.MerchantID, cmd.PaymentID)
		if gerr != nil {
			return gerr
		}
		if cur.Version != loadedVersion {
			return pgdb.ErrConcurrentModification
		}
		ev, verr := cur.Void(domain.VoidReasonMerchantRequest, cmd.Reason, s.clock.Now())
		if verr != nil {
			return verr
		}
		p = cur
		return s.persist(ctx, cur, loadedVersion, nil, []domain.Event{ev})
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ConfirmCommand 為 3DS / 導向完成後的確認輸入。
type ConfirmCommand struct {
	MerchantID     string
	PaymentID      string
	IdempotencyKey string
	ProviderParams map[string]string
}

// ConfirmPayment 於 requires_action 後向 adapter 查詢結果並套用（成功 → authorized / captured；失敗 → failed）。
// 重複確認回傳目前 Payment。
func (s *Service) ConfirmPayment(ctx context.Context, cmd ConfirmCommand) (*domain.Payment, error) {
	p, err := s.repo.GetPayment(ctx, cmd.MerchantID, cmd.PaymentID)
	if err != nil {
		return nil, err
	}
	if p.Status != domain.StatusRequiresAction {
		if p.Status == domain.StatusAuthorized || p.Status == domain.StatusCaptured || p.Status == domain.StatusFailed {
			return p, nil
		}
		return nil, domain.TransitionError(p.Status, domain.StatusAuthorized)
	}
	attempt := p.OpenAttempt()
	if attempt == nil {
		return nil, domain.ErrInvalidTransition.WithMessage("payment has no open attempt to confirm")
	}
	client, ok := s.providerFor(p.SelectedProvider)
	if !ok {
		return nil, domain.ErrProviderUnavailable
	}
	loadedVersion := p.Version
	now := s.clock.Now()

	cctx, cancel := s.callCtx(ctx)
	resp, perr := client.GetPaymentStatus(cctx, &providerv1.GetPaymentStatusRequest{PaymentId: p.PublicID, ProviderReference: p.ProviderReference, TestMode: !p.LiveMode})
	cancel()
	if ok, cat, code, msg := resultCategory(resp.GetResult(), perr); !ok {
		if cat == domain.CategoryProviderTimeout || cat == domain.CategoryUnknown {
			return nil, domain.ErrProviderTimeout
		}
		return nil, domain.ProviderError(cat, code, msg)
	}

	var out authOutcome
	switch resp.GetStatus() {
	case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_AUTHORIZED:
		out = authOutcome{kind: outcomeApproved, ref: p.ProviderReference}
	case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_CAPTURED:
		out = authOutcome{kind: outcomeApproved, ref: p.ProviderReference, captured: true}
		if m, err := money.FromProto(resp.GetCapturedAmount()); err == nil && m.Currency == p.Amount.Currency && m.IsPositive() {
			out.amount = m
		}
	case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_REQUIRES_ACTION, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_PENDING:
		// 付款人尚未完成：維持 requires_action。
		return p, nil
	case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_FAILED, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_EXPIRED,
		providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_NOT_FOUND, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_VOIDED:
		out = authOutcome{kind: outcomeFailed, category: domain.CategoryAuthenticationFailed, code: "authentication_failed", message: "customer authentication failed or was abandoned"}
		if resp.GetFailureCode() != "" {
			out.code = resp.GetFailureCode()
		}
	case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_PARTIALLY_CAPTURED, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_REFUNDED,
		providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_PARTIALLY_REFUNDED, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_UNSPECIFIED:
		return p, nil
	}

	if out.kind == outcomeFailed {
		attempt.MarkFailed(out.category, out.code, out.message, "", now)
		ev, err := p.Fail(domain.Failure{Category: out.category, Code: out.code, Message: out.message, Provider: attempt.Provider}, now)
		if err != nil {
			return nil, err
		}
		if err := s.tx.WithinTx(context.WithoutCancel(ctx), func(ctx context.Context) error {
			return s.persist(ctx, p, loadedVersion, []*domain.Attempt{attempt}, []domain.Event{ev})
		}); err != nil {
			return nil, err
		}
		return p, nil
	}
	attempt.MarkApproved(out.ref, now)
	if err := s.applyApproved(ctx, p, attempt, out, now); err != nil {
		if errors.Is(err, domain.ErrPaymentExpired) {
			return nil, err
		}
		return nil, err
	}
	return p, nil
}

// CreateRefundCommand 為退款輸入。
type CreateRefundCommand struct {
	MerchantID     string
	PaymentID      string
	IdempotencyKey string
	RequestHash    string
	// Amount 為 nil 時退還全部可退金額。
	Amount   *money.Money
	Reason   string
	Metadata map[string]string
}

// CreateRefundResult 為退款結果。
type CreateRefundResult struct {
	Refund   *domain.Refund
	Replayed bool
}

// CreateRefund 兩階段退款（docs/02 §5.3）：tx1 鎖 payment + 預留額度 + refund(pending) → adapter Refund → tx2 套用結果。
func (s *Service) CreateRefund(ctx context.Context, cmd CreateRefundCommand) (*CreateRefundResult, error) {
	if cmd.IdempotencyKey == "" {
		return nil, domain.ErrPaymentMethodMissing.WithMessage("idempotency_key is required").WithParam("idempotency_key")
	}
	if existing, err := s.repo.GetRefundByIdempotencyKey(ctx, cmd.MerchantID, cmd.IdempotencyKey); err == nil {
		return &CreateRefundResult{Refund: existing, Replayed: true}, nil
	} else if !errors.Is(err, domain.ErrRefundNotFound) {
		return nil, err
	}

	var (
		refund *domain.Refund
		p      *domain.Payment
	)
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		cur, gerr := s.repo.GetPaymentForUpdate(ctx, cmd.MerchantID, cmd.PaymentID)
		if gerr != nil {
			return gerr
		}
		now := s.clock.Now()
		amount := cur.AvailableToRefund()
		if cmd.Amount != nil {
			amount = *cmd.Amount
		}
		r, err := domain.NewRefund(cur, cmd.IdempotencyKey, amount, cmd.Reason, cmd.Metadata, now)
		if err != nil {
			return err
		}
		base := cur.Version
		ev, err := cur.ReserveRefund(r, now)
		if err != nil {
			return err
		}
		if err := s.repo.CreateRefund(ctx, r); err != nil {
			return err
		}
		if err := s.persist(ctx, cur, base, nil, []domain.Event{ev}); err != nil {
			return err
		}
		refund, p = r, cur
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrDuplicateIdempotencyKey) {
			existing, gerr := s.repo.GetRefundByIdempotencyKey(ctx, cmd.MerchantID, cmd.IdempotencyKey)
			if gerr != nil {
				return nil, gerr
			}
			return &CreateRefundResult{Refund: existing, Replayed: true}, nil
		}
		return nil, err
	}

	// 外部呼叫（無 DB 鎖）。
	client, ok := s.providerFor(p.SelectedProvider)
	var (
		succeeded bool
		cat       domain.ProviderErrorCategory
		code, msg string
		resp      *providerv1.RefundResponse
	)
	if !ok {
		cat, code, msg = domain.CategoryProviderUnavailable, "provider_not_configured", "provider is not configured"
	} else {
		cctx, cancel := s.callCtx(ctx)
		var perr error
		resp, perr = client.Refund(cctx, &providerv1.RefundRequest{
			PaymentId: p.PublicID, RefundId: refund.PublicID, ProviderReference: p.ProviderReference, IdempotencyKey: refund.PublicID,
			Amount: refund.Amount.ToProto(), Reason: refund.Reason, Metadata: refund.Metadata, TestMode: !p.LiveMode,
		})
		cancel()
		succeeded, cat, code, msg = resultCategory(resp.GetResult(), perr)
		if succeeded && resp.GetStatus() == providerv1.RefundState_REFUND_STATE_FAILED {
			succeeded, cat, code, msg = false, domain.CategoryDeclinedHard, "refund_failed", "provider reported the refund as failed"
		}
	}
	if !succeeded && (cat == domain.CategoryProviderTimeout || cat == domain.CategoryUnknown) {
		// 結果不明：Refund 留在 pending，由 refund-reconciler 收斂（TODO）。
		s.log.Warn("refund outcome unknown; leaving pending", "refund_id", refund.PublicID, "err", msg)
		return &CreateRefundResult{Refund: refund}, nil
	}
	if succeeded && resp.GetStatus() == providerv1.RefundState_REFUND_STATE_PENDING {
		// 非同步退款：等 webhook（TODO: ingest provider webhook）。
		return &CreateRefundResult{Refund: refund}, nil
	}

	// tx2：套用結果。
	err = s.tx.WithinTx(context.WithoutCancel(ctx), func(ctx context.Context) error {
		cur, gerr := s.repo.GetPaymentForUpdate(ctx, cmd.MerchantID, cmd.PaymentID)
		if gerr != nil {
			return gerr
		}
		now := s.clock.Now()
		base := cur.Version
		rv := refund.Version
		var (
			ev   domain.Event
			terr error
		)
		if succeeded {
			if serr := refund.Succeed(resp.GetProviderRefundReference(), now); serr != nil {
				return serr
			}
			if ev, terr = cur.MarkRefunded(refund, now); terr != nil {
				return terr
			}
		} else {
			if ferr := refund.Fail(cat.RESTCode(), firstNonEmpty(msg, code), now); ferr != nil {
				return ferr
			}
			if ev, terr = cur.ReleaseRefund(refund, now); terr != nil {
				return terr
			}
		}
		if uerr := s.repo.UpdateRefund(ctx, refund, rv); uerr != nil {
			return uerr
		}
		return s.persist(ctx, cur, base, nil, []domain.Event{ev})
	})
	if err != nil {
		return nil, err
	}
	return &CreateRefundResult{Refund: refund}, nil
}

// GetRefund 依公開 ID 取得退款。
func (s *Service) GetRefund(ctx context.Context, merchantID, refundID string) (*domain.Refund, error) {
	return s.repo.GetRefund(ctx, merchantID, refundID)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

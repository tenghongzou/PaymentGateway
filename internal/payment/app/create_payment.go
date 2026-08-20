package app

import (
	"context"
	"errors"
	"time"

	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/money"
)

// CreatePaymentCommand 為建立付款的輸入（由 gRPC adapter 從 CreatePaymentRequest 轉換）。
type CreatePaymentCommand struct {
	MerchantID          string
	IdempotencyKey      string
	RequestHash         string
	Amount              money.Money
	CaptureMethod       domain.CaptureMethod
	PaymentMethodType   string
	Instrument          *providerv1.PaymentInstrument
	MethodDetails       domain.PaymentMethodDetails
	Customer            domain.Customer
	Description         string
	StatementDescriptor string
	ReturnURL           string
	Metadata            map[string]string
	LiveMode            bool
	ThreeDS             providerv1.ThreeDsPreference
	PreferredProvider   string
}

// CreatePaymentResult 為建立付款的結果。
type CreatePaymentResult struct {
	Payment  *domain.Payment
	Replayed bool
}

// CreatePayment 建立付款並同步執行授權（路由 → Authorize → failover 迴圈）。
//
// 冪等：(merchant_id, idempotency_key) 已存在 → 比對 request hash，相同回既有 Payment（Replayed=true），不同回 idempotency_key_payload_mismatch。
// 業務拒絕（decline）不回 error：回傳 status=failed 的 Payment。
func (s *Service) CreatePayment(ctx context.Context, cmd CreatePaymentCommand) (*CreatePaymentResult, error) {
	if cmd.IdempotencyKey == "" {
		return nil, domain.ErrPaymentMethodMissing.WithMessage("idempotency_key is required").WithParam("idempotency_key")
	}
	if cmd.Instrument == nil {
		return nil, domain.ErrPaymentMethodMissing
	}
	// 1. 冪等：先查既有。
	if existing, err := s.repo.GetPaymentByIdempotencyKey(ctx, cmd.MerchantID, cmd.IdempotencyKey); err == nil {
		return s.replay(existing, cmd.RequestHash)
	} else if !errors.Is(err, domain.ErrPaymentNotFound) {
		return nil, err
	}

	now := s.clock.Now()
	p, created, err := domain.NewPayment(domain.NewPaymentParams{
		MerchantID: cmd.MerchantID, IdempotencyKey: cmd.IdempotencyKey, RequestHash: cmd.RequestHash,
		Amount: cmd.Amount, CaptureMethod: cmd.CaptureMethod, PaymentMethodType: cmd.PaymentMethodType,
		PaymentMethod: cmd.MethodDetails, Customer: cmd.Customer, Description: cmd.Description,
		StatementDescriptor: cmd.StatementDescriptor, ReturnURL: cmd.ReturnURL, Metadata: cmd.Metadata, LiveMode: cmd.LiveMode,
	}, now)
	if err != nil {
		return nil, err
	}

	// 2. 路由（無候選 → 422 no_route_available，不建立 payment）。
	candidates, err := s.router.Route(ctx, RoutingContext{
		MerchantID: cmd.MerchantID, Amount: cmd.Amount, PaymentMethodType: cmd.PaymentMethodType,
		TokenProvider: cmd.MethodDetails.TokenProvider, CaptureMethod: p.CaptureMethod,
		PreferredProvider: cmd.PreferredProvider, LiveMode: cmd.LiveMode,
	})
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, domain.ErrNoRouteAvailable
	}

	// 3. tx1：建立聚合根 + 第一個 attempt（唯一索引在此生效）。
	attempt, err := p.StartAttempt(candidates[0].Provider, candidates[0].Reason, now)
	if err != nil {
		return nil, err
	}
	err = s.tx.WithinTx(ctx, func(ctx context.Context) error {
		if cerr := s.repo.CreatePayment(ctx, p); cerr != nil {
			return cerr
		}
		if ierr := s.repo.InsertAttempt(ctx, attempt); ierr != nil {
			return ierr
		}
		return s.appendEvents(ctx, p, []domain.Event{created})
	})
	if err != nil {
		if errors.Is(err, ErrDuplicateIdempotencyKey) {
			existing, gerr := s.repo.GetPaymentByIdempotencyKey(ctx, cmd.MerchantID, cmd.IdempotencyKey)
			if gerr != nil {
				return nil, gerr
			}
			return s.replay(existing, cmd.RequestHash)
		}
		return nil, err
	}

	// 4. authorize saga（failover 迴圈），整體預算 25s。
	sagaCtx, cancel := context.WithTimeout(ctx, s.cfg.SagaBudget)
	defer cancel()
	if err := s.runAuthorizeSaga(sagaCtx, p, cmd, candidates); err != nil {
		return nil, err
	}
	return &CreatePaymentResult{Payment: p}, nil
}

func (s *Service) replay(existing *domain.Payment, requestHash string) (*CreatePaymentResult, error) {
	if existing.IdempotencyRequestHash != "" && requestHash != "" && existing.IdempotencyRequestHash != requestHash {
		return nil, domain.ErrIdempotencyKeyMismatch
	}
	return &CreatePaymentResult{Payment: existing, Replayed: true}, nil
}

// runAuthorizeSaga 對候選依序 Authorize，依 ProviderErrorCategory 決定 failover / 結束。
func (s *Service) runAuthorizeSaga(ctx context.Context, p *domain.Payment, cmd CreatePaymentCommand, candidates []Candidate) error {
	log := s.log.With("payment_id", p.PublicID, "merchant_id", p.MerchantID)
	candidateIdx := 0
	for {
		attempt := p.CurrentAttempt()
		out := s.authorizeOnce(ctx, p, attempt, cmd)
		now := s.clock.Now()

		// unknown（timeout）：先以 GetPaymentStatus 收斂，最多 3 次（1s/2s/4s）。
		if out.kind == outcomeFailed && out.category.AttemptStatus() == domain.AttemptUnknown {
			out = s.resolveUnknown(ctx, p, attempt, out)
			now = s.clock.Now()
		}

		switch out.kind {
		case outcomeApproved:
			attempt.MarkApproved(out.ref, now)
			s.router.Report(attempt.Provider, domain.CategoryNone)
			return s.applyApproved(ctx, p, attempt, out, now)

		case outcomeRequiresAction:
			attempt.MarkRequiresAction(out.ref, out.nextAction, now)
			s.router.Report(attempt.Provider, domain.CategoryNone)
			expires := time.Time{}
			if out.nextAction != nil {
				expires = out.nextAction.ExpiresAt
			}
			ev, err := p.RequireAction(out.ref, expires, now)
			if err != nil {
				return err
			}
			return s.tx.WithinTx(ctx, func(ctx context.Context) error {
				return s.persist(ctx, p, p.Version-1, []*domain.Attempt{attempt}, []domain.Event{ev})
			})

		case outcomeFailed:
			attempt.MarkFailed(out.category, out.code, out.message, out.ref, now)
			s.router.Report(attempt.Provider, out.category)
			if attempt.Status == domain.AttemptUnknown {
				// 仍不明：payment 維持 created，交給 sweeper（docs/02 T5）；絕不 failover。
				log.Warn("authorize outcome unknown; leaving payment created for reconciliation", "attempt", attempt.AttemptNo, "provider", attempt.Provider)
				return s.tx.WithinTx(ctx, func(ctx context.Context) error {
					return s.persist(ctx, p, p.Version, []*domain.Attempt{attempt}, nil)
				})
			}
			next := s.nextCandidate(ctx, p, attempt, candidates, &candidateIdx)
			if next == nil {
				ev, err := p.Fail(domain.Failure{Category: out.category, Code: out.code, Message: out.message, Provider: attempt.Provider}, now)
				if err != nil {
					return err
				}
				log.Info("payment failed", "category", out.category, "code", out.code, "attempts", len(p.Attempts))
				return s.tx.WithinTx(ctx, func(ctx context.Context) error {
					return s.persist(ctx, p, p.Version-1, []*domain.Attempt{attempt}, []domain.Event{ev})
				})
			}
			nextAttempt, err := p.StartAttempt(next.Provider, next.Reason, now)
			if err != nil {
				return err
			}
			log.Info("failover", "from", attempt.Provider, "to", next.Provider, "reason", next.Reason, "category", out.category)
			if err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
				if err := s.persist(ctx, p, p.Version, []*domain.Attempt{attempt}, nil); err != nil {
					return err
				}
				return s.repo.InsertAttempt(ctx, nextAttempt)
			}); err != nil {
				return err
			}
		}
	}
}

// authorizeOnce 呼叫 adapter 的 Authorize 並正規化結果。
func (s *Service) authorizeOnce(ctx context.Context, p *domain.Payment, attempt *domain.Attempt, cmd CreatePaymentCommand) authOutcome {
	client, ok := s.providerFor(attempt.Provider)
	if !ok {
		return authOutcome{kind: outcomeFailed, category: domain.CategoryProviderUnavailable, code: "provider_not_configured", message: "provider " + attempt.Provider + " is not configured"}
	}
	req := &providerv1.AuthorizeRequest{
		PaymentId: p.PublicID, MerchantId: p.MerchantID, IdempotencyKey: attempt.PublicID(),
		Amount: p.Amount.ToProto(), CaptureImmediately: p.CaptureMethod == domain.CaptureAutomatic,
		Instrument: cmd.Instrument, ReturnUrl: p.ReturnURL, StatementDescriptor: p.StatementDescriptor,
		Description: p.Description, ThreeDs: cmd.ThreeDS, Metadata: p.Metadata, TestMode: !p.LiveMode,
		Customer: &providerv1.CustomerInfo{
			Id: p.Customer.ID, Email: p.Customer.Email, Name: p.Customer.Name, Phone: p.Customer.Phone,
			IpAddress: p.Customer.IPAddress, UserAgent: p.Customer.UserAgent, BillingCountry: p.Customer.BillingCountry, BillingPostalCode: p.Customer.BillingPostalCode,
		},
	}
	cctx, cancel := s.callCtx(ctx)
	defer cancel()
	resp, err := client.Authorize(cctx, req)
	return evaluateAuthorize(resp, err, p.Amount.Currency)
}

// resolveUnknown 以 GetPaymentStatus 收斂 timeout（docs/05 §3.3 第 3 點）。
func (s *Service) resolveUnknown(ctx context.Context, p *domain.Payment, attempt *domain.Attempt, out authOutcome) authOutcome {
	client, ok := s.providerFor(attempt.Provider)
	if !ok {
		return out
	}
	for _, d := range s.cfg.ResolveDelays {
		if !sleepCtx(ctx, d) {
			return out
		}
		cctx, cancel := s.callCtx(ctx)
		resp, err := client.GetPaymentStatus(cctx, &providerv1.GetPaymentStatusRequest{PaymentId: p.PublicID, ProviderReference: attempt.ProviderReference, TestMode: !p.LiveMode})
		cancel()
		if err != nil {
			continue
		}
		switch resp.GetStatus() {
		case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_AUTHORIZED:
			return authOutcome{kind: outcomeApproved, ref: resp.GetResult().GetProviderReference()}
		case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_CAPTURED:
			return authOutcome{kind: outcomeApproved, ref: resp.GetResult().GetProviderReference(), captured: true}
		case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_NOT_FOUND:
			return authOutcome{kind: outcomeFailed, category: domain.CategoryProviderUnavailable, code: "timeout_not_found", message: "provider timed out and has no record of the authorization"}
		case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_FAILED, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_EXPIRED,
			providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_VOIDED:
			return authOutcome{kind: outcomeFailed, category: domain.CategoryDeclinedHard, code: resp.GetFailureCode(), message: "provider reported the authorization as failed"}
		case providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_PENDING, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_REQUIRES_ACTION,
			providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_PARTIALLY_CAPTURED, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_REFUNDED,
			providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_PARTIALLY_REFUNDED, providerv1.ProviderPaymentStatus_PROVIDER_PAYMENT_STATUS_UNSPECIFIED:
			continue
		}
	}
	return out
}

// applyApproved 套用授權成功（automatic 時同一 tx 內 authorized + captured，docs/02 T3）。
func (s *Service) applyApproved(ctx context.Context, p *domain.Payment, attempt *domain.Attempt, out authOutcome, now time.Time) error {
	base := p.Version
	evAuth, err := p.Authorize(domain.AuthorizeParams{Provider: attempt.Provider, ProviderReference: out.ref, AuthExpiresAt: out.authExpiry, Details: out.details, FeeMinor: out.fee}, now)
	if err != nil {
		return err
	}
	events := []domain.Event{evAuth}
	if p.CaptureMethod == domain.CaptureAutomatic {
		amount := p.Amount
		if out.amount.IsPositive() {
			amount = out.amount
		}
		if !out.captured {
			// adapter 只授權未請款（不支援 capture_immediately）：立即補一次 Capture。
			if cerr := s.captureNow(ctx, p, amount); cerr != nil {
				s.log.Warn("immediate capture after authorize failed; payment stays authorized", "payment_id", p.PublicID, "err", cerr)
				return s.tx.WithinTx(ctx, func(ctx context.Context) error {
					return s.persist(ctx, p, base, []*domain.Attempt{attempt}, events)
				})
			}
		}
		evCap, err := p.Capture(amount, out.fee, now)
		if err != nil {
			return err
		}
		events = append(events, evCap)
	}
	return s.tx.WithinTx(ctx, func(ctx context.Context) error {
		return s.persist(ctx, p, base, []*domain.Attempt{attempt}, events)
	})
}

// captureNow 在 authorize 之後立刻呼叫 adapter Capture（automatic 但 adapter 未一次完成時）。
func (s *Service) captureNow(ctx context.Context, p *domain.Payment, amount money.Money) error {
	client, ok := s.providerFor(p.SelectedProvider)
	if !ok {
		return domain.ErrProviderUnavailable
	}
	cctx, cancel := s.callCtx(ctx)
	defer cancel()
	resp, err := client.Capture(cctx, &providerv1.CaptureRequest{PaymentId: p.PublicID, ProviderReference: p.ProviderReference, IdempotencyKey: p.PublicID + ":capture:1", Amount: amount.ToProto(), Final: true, TestMode: !p.LiveMode})
	ok, cat, code, msg := resultCategory(resp.GetResult(), err)
	if !ok {
		return domain.ProviderError(cat, code, msg)
	}
	return nil
}

// nextCandidate 依 failover 規則挑下一個候選（docs/02 §9.5）；無則回 nil。
func (s *Service) nextCandidate(ctx context.Context, p *domain.Payment, last *domain.Attempt, candidates []Candidate, idx *int) *Candidate {
	if !last.CanFailover() {
		return nil
	}
	if len(p.Attempts) >= s.cfg.MaxAttempts {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < s.cfg.MinRemainingForFailover {
		return nil
	}
	for *idx+1 < len(candidates) {
		*idx++
		c := candidates[*idx]
		if c.Provider != last.Provider {
			c.Reason = "fallback"
			return &c
		}
	}
	if s.cfg.RetrySameProviderOnUnavailable && last.Status == domain.AttemptUnavailable {
		return &Candidate{Provider: last.Provider, Reason: "retry"}
	}
	return nil
}

// sleepCtx 等待 d 或 ctx 結束；ctx 結束回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

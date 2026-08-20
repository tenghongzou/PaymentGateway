// Package postgres 實作 app.PaymentRepo / app.TxManager / app.OutboxStore（pgx，SQL 對齊 migrations/payment）。
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/payment/app"
	"github.com/tenghongzou/paymentgateway/internal/payment/domain"
	"github.com/tenghongzou/paymentgateway/pkg/ids"
	"github.com/tenghongzou/paymentgateway/pkg/money"
	"github.com/tenghongzou/paymentgateway/pkg/outbox"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

type txKey struct{}

// TxManager 實作 app.TxManager：把 pgx.Tx 放進 ctx 供 Repo / OutboxStore 使用。
type TxManager struct {
	pool *pgxpool.Pool
}

// NewTxManager 建立 TxManager。
func NewTxManager(pool *pgxpool.Pool) *TxManager { return &TxManager{pool: pool} }

// WithinTx 實作 app.TxManager（已在交易內時直接重用）。
func (m *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}
	return pgdb.WithTx(ctx, m.pool, func(tx pgx.Tx) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

// TxFromContext 取出交易（沒有時回 nil）。
func TxFromContext(ctx context.Context) pgx.Tx {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	if !ok {
		return nil
	}
	return tx
}

// Repo 實作 app.PaymentRepo。
type Repo struct {
	pool *pgxpool.Pool
}

// NewRepo 建立 Repo。
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

func (r *Repo) q(ctx context.Context) dbtx {
	if tx := TxFromContext(ctx); tx != nil {
		return tx
	}
	return r.pool
}

// dbtx 為 pgxpool.Pool 與 pgx.Tx 共同滿足的最小介面。
type dbtx interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconnCommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OutboxStore 實作 app.OutboxStore（需在交易內）。
type OutboxStore struct {
	store *outbox.Store
}

// NewOutboxStore 建立 OutboxStore。
func NewOutboxStore() *OutboxStore { return &OutboxStore{store: outbox.NewStore()} }

// Insert 實作 app.OutboxStore。
func (o *OutboxStore) Insert(ctx context.Context, msg outbox.Message) error {
	tx := TxFromContext(ctx)
	if tx == nil {
		return errors.New("postgres: outbox insert requires a transaction")
	}
	_, err := o.store.Insert(ctx, tx, msg)
	return err
}

// ---- payments ----

const paymentColumns = `id::text, public_id, merchant_id::text, idempotency_key, COALESCE(idempotency_request_hash, ''), amount, currency, capture_method, status,
	amount_authorized, amount_captured, amount_refunded, amount_refund_pending, payment_method_type, payment_method_details, customer,
	COALESCE(description, ''), COALESCE(statement_descriptor, ''), COALESCE(return_url, ''), COALESCE(selected_provider, ''), COALESCE(provider_reference, ''),
	failure_category, failure_code, failure_message, expires_at, auth_expires_at, authorized_at, captured_at, void_reason, voided_at, metadata, created_at, updated_at, version`

func scanPayment(row pgx.Row) (*domain.Payment, error) {
	var (
		p                                      domain.Payment
		currency                               string
		amount, auth, captured, refunded, pend int64
		details, customer, metadata            []byte
		failCat, failCode, failMsg, voidReason *string
	)
	err := row.Scan(&p.ID, &p.PublicID, &p.MerchantID, &p.IdempotencyKey, &p.IdempotencyRequestHash, &amount, &currency, &p.CaptureMethod, &p.Status,
		&auth, &captured, &refunded, &pend, &p.PaymentMethodType, &details, &customer,
		&p.Description, &p.StatementDescriptor, &p.ReturnURL, &p.SelectedProvider, &p.ProviderReference,
		&failCat, &failCode, &failMsg, &p.ExpiresAt, &p.AuthExpiresAt, &p.AuthorizedAt, &p.CapturedAt, &voidReason, &p.VoidedAt, &metadata, &p.CreatedAt, &p.UpdatedAt, &p.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPaymentNotFound
		}
		return nil, fmt.Errorf("postgres: scan payment: %w", err)
	}
	p.Amount = money.Money{AmountMinor: amount, Currency: currency}
	p.AmountAuthorized = money.Money{AmountMinor: auth, Currency: currency}
	p.AmountCaptured = money.Money{AmountMinor: captured, Currency: currency}
	p.AmountRefunded = money.Money{AmountMinor: refunded, Currency: currency}
	p.AmountRefundPending = money.Money{AmountMinor: pend, Currency: currency}
	if err := unmarshalJSONB(details, &p.PaymentMethodDetails); err != nil {
		return nil, err
	}
	if err := unmarshalJSONB(customer, &p.Customer); err != nil {
		return nil, err
	}
	if err := unmarshalJSONB(metadata, &p.Metadata); err != nil {
		return nil, err
	}
	if p.Metadata == nil {
		p.Metadata = map[string]string{}
	}
	if failCat != nil || failCode != nil {
		p.Failure = &domain.Failure{Category: domain.ProviderErrorCategory(deref(failCat)), Code: deref(failCode), Message: deref(failMsg), Provider: p.SelectedProvider}
		p.Failure.Retryable = domain.IsRetryableDecline(p.Failure.Category, p.Failure.Code)
	}
	if voidReason != nil {
		vr := domain.VoidReason(*voidReason)
		p.VoidReason = &vr
	}
	return &p, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// unmarshalJSONB 解析 jsonb 欄位（空值視為 {}）。
func unmarshalJSONB(raw []byte, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("postgres: decode jsonb: %w", err)
	}
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// CreatePayment 實作 app.PaymentRepo。
func (r *Repo) CreatePayment(ctx context.Context, p *domain.Payment) error {
	_, err := r.q(ctx).Exec(ctx, `
		INSERT INTO payments (id, public_id, merchant_id, idempotency_key, idempotency_request_hash, amount, currency, capture_method, status,
			amount_authorized, amount_captured, amount_refunded, amount_refund_pending, payment_method_type, payment_method_details, customer,
			description, statement_descriptor, return_url, selected_provider, provider_reference, expires_at, metadata, created_at, updated_at, version)
		VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb, $16::jsonb, $17, $18, $19, $20, $21, $22, $23::jsonb, $24, $25, $26)`,
		p.ID, p.PublicID, p.MerchantID, p.IdempotencyKey, nullStr(p.IdempotencyRequestHash), p.Amount.AmountMinor, p.Amount.Currency, string(p.CaptureMethod), string(p.Status),
		p.AmountAuthorized.AmountMinor, p.AmountCaptured.AmountMinor, p.AmountRefunded.AmountMinor, p.AmountRefundPending.AmountMinor, p.PaymentMethodType, mustJSON(p.PaymentMethodDetails), mustJSON(p.Customer),
		nullStr(p.Description), nullStr(p.StatementDescriptor), nullStr(p.ReturnURL), nullStr(p.SelectedProvider), nullStr(p.ProviderReference), p.ExpiresAt, mustJSON(p.Metadata), p.CreatedAt, p.UpdatedAt, p.Version)
	if err != nil {
		if pgdb.IsUniqueViolation(err) && pgdb.ConstraintName(err) == "payments_merchant_idem_key" {
			return app.ErrDuplicateIdempotencyKey
		}
		return fmt.Errorf("postgres: insert payment: %w", err)
	}
	return nil
}

func (r *Repo) getPayment(ctx context.Context, where string, forUpdate bool, args ...any) (*domain.Payment, error) {
	sql := `SELECT ` + paymentColumns + ` FROM payments WHERE ` + where
	if forUpdate {
		sql += " FOR UPDATE"
	}
	p, err := scanPayment(r.q(ctx).QueryRow(ctx, sql, args...))
	if err != nil {
		return nil, err
	}
	if err := r.loadAttempts(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// GetPayment 實作 app.PaymentRepo。
func (r *Repo) GetPayment(ctx context.Context, merchantID, publicID string) (*domain.Payment, error) {
	return r.getPayment(ctx, "merchant_id = $1::uuid AND public_id = $2", false, merchantID, publicID)
}

// GetPaymentByIdempotencyKey 實作 app.PaymentRepo。
func (r *Repo) GetPaymentByIdempotencyKey(ctx context.Context, merchantID, key string) (*domain.Payment, error) {
	return r.getPayment(ctx, "merchant_id = $1::uuid AND idempotency_key = $2", false, merchantID, key)
}

// GetPaymentForUpdate 實作 app.PaymentRepo。
func (r *Repo) GetPaymentForUpdate(ctx context.Context, merchantID, publicID string) (*domain.Payment, error) {
	if TxFromContext(ctx) == nil {
		return nil, errors.New("postgres: GetPaymentForUpdate requires a transaction")
	}
	return r.getPayment(ctx, "merchant_id = $1::uuid AND public_id = $2", true, merchantID, publicID)
}

// UpdatePayment 實作 app.PaymentRepo（樂觀鎖）。
func (r *Repo) UpdatePayment(ctx context.Context, p *domain.Payment, expectedVersion int) error {
	var failCat, failCode, failMsg *string
	if p.Failure != nil {
		failCat, failCode, failMsg = nullStr(string(p.Failure.Category)), nullStr(p.Failure.PublicCode()), nullStr(p.Failure.Message)
	}
	var voidReason *string
	if p.VoidReason != nil {
		voidReason = nullStr(string(*p.VoidReason))
	}
	tag, err := r.q(ctx).Exec(ctx, `
		UPDATE payments SET status = $3, amount_authorized = $4, amount_captured = $5, amount_refunded = $6, amount_refund_pending = $7,
			payment_method_details = $8::jsonb, selected_provider = $9, provider_reference = $10,
			failure_category = $11, failure_code = $12, failure_message = $13,
			expires_at = $14, auth_expires_at = $15, authorized_at = $16, captured_at = $17, void_reason = $18, voided_at = $19, version = $20
		WHERE id = $1::uuid AND version = $2`,
		p.ID, expectedVersion, string(p.Status), p.AmountAuthorized.AmountMinor, p.AmountCaptured.AmountMinor, p.AmountRefunded.AmountMinor, p.AmountRefundPending.AmountMinor,
		mustJSON(p.PaymentMethodDetails), nullStr(p.SelectedProvider), nullStr(p.ProviderReference),
		failCat, failCode, failMsg, p.ExpiresAt, p.AuthExpiresAt, p.AuthorizedAt, p.CapturedAt, voidReason, p.VoidedAt, p.Version)
	if err != nil {
		return fmt.Errorf("postgres: update payment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgdb.ErrConcurrentModification
	}
	return nil
}

// ---- attempts ----

const attemptColumns = `id::text, payment_id::text, merchant_id::text, attempt_no, operation, provider, COALESCE(provider_reference, ''), status,
	COALESCE(error_category, ''), COALESCE(error_code, ''), COALESCE(error_message, ''), request_snapshot, response_snapshot, latency_ms, created_at, completed_at`

type responseSnapshot struct {
	NextAction *domain.NextAction `json:"next_action,omitempty"`
}

func (r *Repo) loadAttempts(ctx context.Context, p *domain.Payment) error {
	rows, err := r.q(ctx).Query(ctx, `SELECT `+attemptColumns+` FROM payment_attempts WHERE payment_id = $1::uuid ORDER BY attempt_no`, p.ID)
	if err != nil {
		return fmt.Errorf("postgres: load attempts: %w", err)
	}
	defer rows.Close()
	p.Attempts = nil
	for rows.Next() {
		var (
			a              domain.Attempt
			reqSnap, respS []byte
			routeSnap      map[string]any
		)
		if err := rows.Scan(&a.ID, &a.PaymentID, &a.MerchantID, &a.AttemptNo, &a.Operation, &a.Provider, &a.ProviderReference, &a.Status,
			&a.ErrorCategory, &a.ErrorCode, &a.ErrorMessage, &reqSnap, &respS, &a.LatencyMs, &a.CreatedAt, &a.CompletedAt); err != nil {
			return fmt.Errorf("postgres: scan attempt: %w", err)
		}
		if err := unmarshalJSONB(reqSnap, &routeSnap); err != nil {
			return err
		}
		if rr, ok := routeSnap["route_reason"].(string); ok {
			a.RouteReason = rr
		}
		var snap responseSnapshot
		if err := unmarshalJSONB(respS, &snap); err != nil {
			return err
		}
		a.NextAction = snap.NextAction
		ac := a
		p.Attempts = append(p.Attempts, &ac)
	}
	return rows.Err()
}

// InsertAttempt 實作 app.PaymentRepo。
func (r *Repo) InsertAttempt(ctx context.Context, a *domain.Attempt) error {
	_, err := r.q(ctx).Exec(ctx, `
		INSERT INTO payment_attempts (id, payment_id, merchant_id, attempt_no, operation, provider, provider_reference, status, error_category, error_code, error_message,
			request_snapshot, response_snapshot, latency_ms, created_at, completed_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13::jsonb, $14, $15, $16)`,
		a.ID, a.PaymentID, a.MerchantID, a.AttemptNo, a.Operation, a.Provider, nullStr(a.ProviderReference), string(a.Status),
		nullStr(string(a.ErrorCategory)), nullStr(a.ErrorCode), nullStr(a.ErrorMessage),
		mustJSON(map[string]any{"route_reason": a.RouteReason}), mustJSON(responseSnapshot{NextAction: a.NextAction}), a.LatencyMs, a.CreatedAt, a.CompletedAt)
	if err != nil {
		return fmt.Errorf("postgres: insert attempt: %w", err)
	}
	return nil
}

// UpdateAttempt 實作 app.PaymentRepo。
func (r *Repo) UpdateAttempt(ctx context.Context, a *domain.Attempt) error {
	_, err := r.q(ctx).Exec(ctx, `
		UPDATE payment_attempts SET provider_reference = $2, status = $3, error_category = $4, error_code = $5, error_message = $6,
			response_snapshot = $7::jsonb, latency_ms = $8, completed_at = $9
		WHERE id = $1::uuid`,
		a.ID, nullStr(a.ProviderReference), string(a.Status), nullStr(string(a.ErrorCategory)), nullStr(a.ErrorCode), nullStr(a.ErrorMessage),
		mustJSON(responseSnapshot{NextAction: a.NextAction}), a.LatencyMs, a.CompletedAt)
	if err != nil {
		return fmt.Errorf("postgres: update attempt: %w", err)
	}
	return nil
}

// ---- events ----

// AppendEvents 實作 app.PaymentRepo（seq = event.Seq = 轉移後的 version）。
func (r *Repo) AppendEvents(ctx context.Context, p *domain.Payment, events []domain.Event, traceID string) error {
	for _, ev := range events {
		var from *string
		if ev.FromStatus != nil {
			s := string(*ev.FromStatus)
			from = &s
		}
		_, err := r.q(ctx).Exec(ctx, `
			INSERT INTO payment_events (id, payment_id, merchant_id, seq, event_type, from_status, to_status, payload, actor, trace_id, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8::jsonb, 'system', $9, $10)`,
			ids.NewUUID().String(), p.ID, p.MerchantID, ev.Seq, ev.Type, from, string(ev.ToStatus), mustJSON(ev.Payload), nullStr(traceID), ev.OccurredAt)
		if err != nil {
			return fmt.Errorf("postgres: insert event %s seq %d: %w", ev.Type, ev.Seq, err)
		}
	}
	return nil
}

// ---- list ----

type cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// ListPayments 實作 app.PaymentRepo（cursor = created_at, id）。
func (r *Repo) ListPayments(ctx context.Context, merchantID string, f app.ListFilter) ([]*domain.Payment, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	args := []any{merchantID}
	where := "merchant_id = $1::uuid"
	add := func(cond string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND %s $%d", cond, len(args))
	}
	if len(f.Statuses) > 0 {
		ss := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			ss[i] = string(s)
		}
		add("status = ANY(", ss)
		where += ")"
	}
	if f.CreatedAfter != nil {
		add("created_at >=", *f.CreatedAfter)
	}
	if f.CreatedBefore != nil {
		add("created_at <", *f.CreatedBefore)
	}
	if f.CustomerID != "" {
		add("customer->>'id' =", f.CustomerID)
	}
	if f.Provider != "" {
		add("selected_provider =", f.Provider)
	}
	if f.Currency != "" {
		add("currency =", f.Currency)
	}
	if f.Cursor != "" {
		var c cursor
		if err := decodeCursor(f.Cursor, &c); err != nil {
			return nil, "", domain.ErrInvalidTransition.WithMessage("invalid cursor").WithParam("cursor")
		}
		args = append(args, c.CreatedAt, c.ID)
		where += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d::uuid)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	rows, err := r.q(ctx).Query(ctx, fmt.Sprintf(`SELECT %s FROM payments WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d`, paymentColumns, where, len(args)), args...)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list payments: %w", err)
	}
	defer rows.Close()
	var out []*domain.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		last := out[limit-1]
		out = out[:limit]
		next = encodeCursor(cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	for _, p := range out {
		if err := r.loadAttempts(ctx, p); err != nil {
			return nil, "", err
		}
	}
	return out, next, nil
}

// ---- refunds ----

const refundColumns = `r.id::text, r.public_id, r.payment_id::text, p.public_id, r.merchant_id::text, r.idempotency_key, r.amount, r.currency, r.status, COALESCE(r.reason, ''),
	COALESCE(r.provider, ''), COALESCE(r.provider_reference, ''), COALESCE(r.failure_code, ''), COALESCE(r.failure_message, ''), r.metadata, r.created_at, r.updated_at, r.succeeded_at, r.version`

func scanRefund(row pgx.Row) (*domain.Refund, error) {
	var (
		rf       domain.Refund
		amount   int64
		currency string
		metadata []byte
	)
	err := row.Scan(&rf.ID, &rf.PublicID, &rf.PaymentID, &rf.PaymentPublicID, &rf.MerchantID, &rf.IdempotencyKey, &amount, &currency, &rf.Status, &rf.Reason,
		&rf.Provider, &rf.ProviderReference, &rf.FailureCode, &rf.FailureMessage, &metadata, &rf.CreatedAt, &rf.UpdatedAt, &rf.SucceededAt, &rf.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRefundNotFound
		}
		return nil, fmt.Errorf("postgres: scan refund: %w", err)
	}
	rf.Amount = money.Money{AmountMinor: amount, Currency: currency}
	if err := unmarshalJSONB(metadata, &rf.Metadata); err != nil {
		return nil, err
	}
	if rf.Metadata == nil {
		rf.Metadata = map[string]string{}
	}
	return &rf, nil
}

// CreateRefund 實作 app.PaymentRepo。
func (r *Repo) CreateRefund(ctx context.Context, rf *domain.Refund) error {
	_, err := r.q(ctx).Exec(ctx, `
		INSERT INTO refunds (id, public_id, payment_id, merchant_id, idempotency_key, amount, currency, status, reason, provider, provider_reference, failure_code, failure_message, metadata, created_at, updated_at, succeeded_at, version)
		VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::jsonb, $15, $16, $17, $18)`,
		rf.ID, rf.PublicID, rf.PaymentID, rf.MerchantID, rf.IdempotencyKey, rf.Amount.AmountMinor, rf.Amount.Currency, string(rf.Status), nullStr(rf.Reason),
		nullStr(rf.Provider), nullStr(rf.ProviderReference), nullStr(rf.FailureCode), nullStr(rf.FailureMessage), mustJSON(rf.Metadata), rf.CreatedAt, rf.UpdatedAt, rf.SucceededAt, rf.Version)
	if err != nil {
		if pgdb.IsUniqueViolation(err) && pgdb.ConstraintName(err) == "refunds_merchant_idem_key" {
			return app.ErrDuplicateIdempotencyKey
		}
		if pgdb.IsCheckViolation(err) {
			return domain.ErrRefundExceedsAvailable.Wrap(err)
		}
		return fmt.Errorf("postgres: insert refund: %w", err)
	}
	return nil
}

// GetRefund 實作 app.PaymentRepo。
func (r *Repo) GetRefund(ctx context.Context, merchantID, publicID string) (*domain.Refund, error) {
	return scanRefund(r.q(ctx).QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds r JOIN payments p ON p.id = r.payment_id WHERE r.merchant_id = $1::uuid AND r.public_id = $2`, merchantID, publicID))
}

// GetRefundByIdempotencyKey 實作 app.PaymentRepo。
func (r *Repo) GetRefundByIdempotencyKey(ctx context.Context, merchantID, key string) (*domain.Refund, error) {
	return scanRefund(r.q(ctx).QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds r JOIN payments p ON p.id = r.payment_id WHERE r.merchant_id = $1::uuid AND r.idempotency_key = $2`, merchantID, key))
}

// UpdateRefund 實作 app.PaymentRepo（樂觀鎖）。
func (r *Repo) UpdateRefund(ctx context.Context, rf *domain.Refund, expectedVersion int) error {
	tag, err := r.q(ctx).Exec(ctx, `
		UPDATE refunds SET status = $3, provider = $4, provider_reference = $5, failure_code = $6, failure_message = $7, succeeded_at = $8, version = $9
		WHERE id = $1::uuid AND version = $2`,
		rf.ID, expectedVersion, string(rf.Status), nullStr(rf.Provider), nullStr(rf.ProviderReference), nullStr(rf.FailureCode), nullStr(rf.FailureMessage), rf.SucceededAt, rf.Version)
	if err != nil {
		return fmt.Errorf("postgres: update refund: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgdb.ErrConcurrentModification
	}
	return nil
}

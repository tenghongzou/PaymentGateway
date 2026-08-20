package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/ledger/app"
	"github.com/tenghongzou/paymentgateway/internal/ledger/domain"
)

// JournalRepo 實作 app.JournalRepo（append-only：只有 INSERT / SELECT）。
type JournalRepo struct {
	pool *pgxpool.Pool
}

// NewJournalRepo 建立 JournalRepo。
func NewJournalRepo(pool *pgxpool.Pool) *JournalRepo { return &JournalRepo{pool: pool} }

// journals 沒有 effective_at 欄位，業務發生時間存於 metadata。
const metaEffectiveAt = "effective_at"

const journalCols = `j.id, j.public_id, j.merchant_id, j.reference_type, j.reference_id, j.event_id, j.description,
	j.reversal_of_journal_id, j.posted_at, j.metadata, j.created_at,
	(SELECT r.id FROM journals r WHERE r.reversal_of_journal_id = j.id ORDER BY r.posted_at LIMIT 1) AS reversed_by`

// Insert 在目前交易內寫入 journal 與 entries。
func (r *JournalRepo) Insert(ctx context.Context, j *domain.Journal) error {
	tx, ok := TxFromContext(ctx)
	if !ok {
		return errors.New("ledger/postgres: journal insert requires a transaction")
	}
	meta := map[string]string{}
	for k, v := range j.Metadata {
		meta[k] = v
	}
	if !j.EffectiveAt.IsZero() {
		meta[metaEffectiveAt] = j.EffectiveAt.UTC().Format(time.RFC3339Nano)
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("ledger/postgres: marshal journal metadata: %w", err)
	}
	var description *string
	if j.Description != "" {
		description = &j.Description
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO journals (id, public_id, merchant_id, reference_type, reference_id, event_id, description,
		                      reversal_of_journal_id, posted_at, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`,
		j.ID, j.PublicID, j.MerchantID, string(j.ReferenceType), j.ReferenceID, j.EventID, description,
		j.ReversalOf, j.PostedAt, metaJSON); err != nil {
		return translateError(fmt.Errorf("ledger/postgres: insert journal: %w", err))
	}
	// entries：逐筆 INSERT（BEFORE INSERT trigger 驗帳戶；deferred trigger 於 commit 驗平衡）。
	// created_at 與 posted_at 相同，落在同一個月分割。
	batch := &pgx.Batch{}
	for i := range j.Entries {
		e := &j.Entries[i]
		e.CreatedAt = j.PostedAt
		batch.Queue(`
			INSERT INTO entries (id, journal_id, account_id, direction, amount, currency, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			e.ID, j.ID, e.AccountID, string(e.Direction), e.Amount.AmountMinor, e.Amount.Currency, e.CreatedAt)
	}
	res := tx.SendBatch(ctx, batch)
	for range j.Entries {
		if _, err := res.Exec(); err != nil {
			_ = res.Close()
			return translateError(fmt.Errorf("ledger/postgres: insert entry: %w", err))
		}
	}
	if err := res.Close(); err != nil {
		return translateError(fmt.Errorf("ledger/postgres: insert entries: %w", err))
	}
	return nil
}

// scanJournal 把一列 journals 轉成 domain.Journal（entries 另外載入）。
func scanJournal(row pgx.Row) (*domain.Journal, error) {
	var (
		j           domain.Journal
		description *string
		meta        []byte
		createdAt   time.Time
	)
	if err := row.Scan(&j.ID, &j.PublicID, &j.MerchantID, &j.ReferenceType, &j.ReferenceID, &j.EventID, &description,
		&j.ReversalOf, &j.PostedAt, &meta, &createdAt, &j.ReversedBy); err != nil {
		return nil, err
	}
	if description != nil {
		j.Description = *description
	}
	j.Metadata = map[string]string{}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &j.Metadata)
	}
	j.Template = j.Metadata[domain.MetaTemplate]
	j.SourceType = domain.SourceType(j.Metadata[domain.MetaSourceType])
	j.SourceID = j.Metadata[domain.MetaSourceID]
	j.Livemode = j.Metadata[domain.MetaLivemode] != "false"
	if s, ok := j.Metadata[metaEffectiveAt]; ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			j.EffectiveAt = t
		}
		delete(j.Metadata, metaEffectiveAt)
	}
	if j.EffectiveAt.IsZero() {
		j.EffectiveAt = j.PostedAt
	}
	return &j, nil
}

// loadEntries 載入多筆 journal 的 entries（以 id 排序 = 插入順序，UUIDv7 單調遞增）。
func (r *JournalRepo) loadEntries(ctx context.Context, journals []*domain.Journal) error {
	if len(journals) == 0 {
		return nil
	}
	byID := make(map[uuid.UUID]*domain.Journal, len(journals))
	idList := make([]uuid.UUID, 0, len(journals))
	for _, j := range journals {
		byID[j.ID] = j
		idList = append(idList, j.ID)
	}
	rows, err := querierFrom(ctx, r.pool).Query(ctx, `
		SELECT e.id, e.journal_id, e.account_id, a.merchant_id, a.code, e.direction, e.amount, e.currency, e.created_at
		  FROM entries e
		  JOIN accounts a ON a.id = e.account_id
		 WHERE e.journal_id = ANY($1)
		 ORDER BY e.journal_id, e.id`, idList)
	if err != nil {
		return fmt.Errorf("ledger/postgres: load entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			e          domain.Entry
			journalID  uuid.UUID
			merchantID *uuid.UUID
			code       string
			currency   string
		)
		if err := rows.Scan(&e.ID, &journalID, &e.AccountID, &merchantID, &code, &e.Direction, &e.Amount.AmountMinor, &currency, &e.CreatedAt); err != nil {
			return fmt.Errorf("ledger/postgres: scan entry: %w", err)
		}
		currency = strings.TrimSpace(currency)
		e.Amount.Currency = currency
		e.Account.Currency = currency
		if merchantID != nil {
			e.Account.MerchantID = *merchantID
		}
		e.Account.Code, e.Account.Livemode = domain.ParseStorageCode(code)
		if j, ok := byID[journalID]; ok {
			j.Entries = append(j.Entries, e)
		}
	}
	return rows.Err()
}

func (r *JournalRepo) getOne(ctx context.Context, where string, arg any) (*domain.Journal, error) {
	row := querierFrom(ctx, r.pool).QueryRow(ctx, `SELECT `+journalCols+` FROM journals j WHERE `+where, arg)
	j, err := scanJournal(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrJournalNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ledger/postgres: get journal: %w", err)
	}
	if err := r.loadEntries(ctx, []*domain.Journal{j}); err != nil {
		return nil, err
	}
	return j, nil
}

// GetByID 取得 journal（含 entries）。
func (r *JournalRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Journal, error) {
	return r.getOne(ctx, "j.id = $1", id)
}

// GetByEventID 以冪等鍵取得 journal。
func (r *JournalRepo) GetByEventID(ctx context.Context, eventID uuid.UUID) (*domain.Journal, error) {
	return r.getOne(ctx, "j.event_id = $1", eventID)
}

// List 依條件列出 journal（posted_at DESC, id DESC；keyset 分頁）。
func (r *JournalRepo) List(ctx context.Context, f app.JournalFilter, page app.Page) ([]*domain.Journal, *app.Cursor, error) {
	var (
		where []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.MerchantID != nil {
		where = append(where, "j.merchant_id = "+arg(*f.MerchantID))
	}
	if f.AccountID != nil {
		where = append(where, "EXISTS (SELECT 1 FROM entries e WHERE e.journal_id = j.id AND e.account_id = "+arg(*f.AccountID)+")")
	}
	if f.ReferenceType != "" {
		where = append(where, "j.reference_type = "+arg(string(f.ReferenceType)))
	}
	if f.ReferenceID != "" {
		where = append(where, "j.reference_id = "+arg(f.ReferenceID))
	}
	if f.SourceType != "" {
		where = append(where, "j.metadata->>'source_type' = "+arg(string(f.SourceType)))
	}
	if f.Template != "" {
		where = append(where, "j.metadata->>'template' = "+arg(f.Template))
	}
	if f.Currency != "" {
		where = append(where, "EXISTS (SELECT 1 FROM entries e WHERE e.journal_id = j.id AND e.currency = "+arg(f.Currency)+")")
	}
	if f.PostedAfter != nil {
		where = append(where, "j.posted_at >= "+arg(*f.PostedAfter))
	}
	if f.PostedBefore != nil {
		where = append(where, "j.posted_at < "+arg(*f.PostedBefore))
	}
	if f.Livemode != nil {
		where = append(where, "COALESCE(j.metadata->>'livemode', 'true') = "+arg(fmt.Sprintf("%t", *f.Livemode)))
	}
	if page.After != nil {
		where = append(where, fmt.Sprintf("(j.posted_at, j.id) < (%s, %s)", arg(page.After.At), arg(page.After.ID)))
	}
	limit := page.Limit
	if limit <= 0 {
		limit = app.DefaultPageSize
	}
	sql := `SELECT ` + journalCols + ` FROM journals j`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY j.posted_at DESC, j.id DESC LIMIT " + arg(limit+1)

	rows, err := querierFrom(ctx, r.pool).Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger/postgres: list journals: %w", err)
	}
	var out []*domain.Journal
	for rows.Next() {
		j, err := scanJournal(rows)
		if err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("ledger/postgres: scan journal: %w", err)
		}
		out = append(out, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("ledger/postgres: list journals rows: %w", err)
	}
	var next *app.Cursor
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = &app.Cursor{At: last.PostedAt, ID: last.ID}
	}
	if err := r.loadEntries(ctx, out); err != nil {
		return nil, nil, err
	}
	return out, next, nil
}

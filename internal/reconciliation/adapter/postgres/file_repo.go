package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenghongzou/paymentgateway/internal/reconciliation/app"
	"github.com/tenghongzou/paymentgateway/internal/reconciliation/domain"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

// FileRepo 實作 app.FileRepo（settlement_files）。
type FileRepo struct{ pool *pgxpool.Pool }

const fileColumns = `id, provider, file_name, file_hash, storage_uri, period_start, period_end, row_count, status,
	error, imported_at, metadata, created_at, updated_at, version`

// Create 實作 app.FileRepo。
func (r *FileRepo) Create(ctx context.Context, f *domain.SettlementFile) error {
	meta, err := json.Marshal(nonNilMap(f.Metadata))
	if err != nil {
		return fmt.Errorf("postgres: marshal file metadata: %w", err)
	}
	_, err = q(ctx, r.pool).Exec(ctx, `
		INSERT INTO settlement_files (id, provider, file_name, file_hash, storage_uri, period_start, period_end,
			row_count, status, error, imported_at, metadata, created_at, updated_at, version)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, $9, NULLIF($10, ''), $11, $12::jsonb, $13, $14, $15)`,
		f.ID, f.Provider, f.FileName, f.FileHash, f.StorageURI, dateOf(f.PeriodStart), dateOf(f.PeriodEnd),
		f.RowCount, string(f.Status), f.Error, f.ImportedAt, meta, f.CreatedAt, f.UpdatedAt, f.Version)
	if err != nil {
		if pgdb.IsUniqueViolation(err) && pgdb.ConstraintName(err) == "settlement_files_hash_key" {
			return domain.ErrDuplicateFile
		}
		return fmt.Errorf("postgres: insert settlement_file: %w", err)
	}
	return nil
}

// GetByHash 實作 app.FileRepo。
func (r *FileRepo) GetByHash(ctx context.Context, hash string) (*domain.SettlementFile, error) {
	row := q(ctx, r.pool).QueryRow(ctx, `SELECT `+fileColumns+` FROM settlement_files WHERE file_hash = $1`, hash)
	return scanFile(row)
}

// GetByID 實作 app.FileRepo。
func (r *FileRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.SettlementFile, error) {
	row := q(ctx, r.pool).QueryRow(ctx, `SELECT `+fileColumns+` FROM settlement_files WHERE id = $1`, id)
	return scanFile(row)
}

// Update 實作 app.FileRepo（樂觀鎖）。
func (r *FileRepo) Update(ctx context.Context, f *domain.SettlementFile) error {
	meta, err := json.Marshal(nonNilMap(f.Metadata))
	if err != nil {
		return fmt.Errorf("postgres: marshal file metadata: %w", err)
	}
	tag, err := q(ctx, r.pool).Exec(ctx, `
		UPDATE settlement_files
		   SET storage_uri = NULLIF($2, ''), period_start = $3, period_end = $4, row_count = $5, status = $6,
		       error = NULLIF($7, ''), imported_at = $8, metadata = $9::jsonb, version = version + 1
		 WHERE id = $1 AND version = $10`,
		f.ID, f.StorageURI, dateOf(f.PeriodStart), dateOf(f.PeriodEnd), f.RowCount, string(f.Status),
		f.Error, f.ImportedAt, meta, f.Version)
	if err := optimistic(tag, err, "settlement_file"); err != nil {
		return err
	}
	f.Version++
	return nil
}

func scanFile(row pgx.Row) (*domain.SettlementFile, error) {
	var (
		f          domain.SettlementFile
		storageURI *string
		errText    *string
		meta       []byte
		status     string
		ps, pe     *time.Time
	)
	err := row.Scan(&f.ID, &f.Provider, &f.FileName, &f.FileHash, &storageURI, &ps, &pe, &f.RowCount, &status,
		&errText, &f.ImportedAt, &meta, &f.CreatedAt, &f.UpdatedAt, &f.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan settlement_file: %w", err)
	}
	f.Status = domain.FileStatus(status)
	f.StorageURI = deref(storageURI)
	f.Error = deref(errText)
	f.PeriodStart, f.PeriodEnd = ps, pe
	f.Metadata = map[string]string{}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &f.Metadata)
	}
	return &f, nil
}

// dateOf 把 *time.Time 轉成 date 欄位值（nil → NULL）。
func dateOf(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

var _ app.FileRepo = (*FileRepo)(nil)

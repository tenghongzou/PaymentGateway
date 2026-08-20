// Package pgdb 提供 pgx 連線池、golang-migrate 包裝、交易 helper 與常見錯誤判斷。
package pgdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// 共用錯誤。
var (
	// ErrNotFound 表示查無資料。
	ErrNotFound = errors.New("pgdb: not found")
	// ErrConcurrentModification 表示樂觀鎖衝突（UPDATE ... WHERE version = $n 影響 0 列）。
	ErrConcurrentModification = errors.New("pgdb: concurrent modification")
)

// PostgreSQL SQLSTATE。
const (
	sqlstateUniqueViolation = "23505"
	sqlstateCheckViolation  = "23514"
	sqlstateFKViolation     = "23503"
)

// Connect 建立連線池並 ping。
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("pgdb: parse url: %w", err)
	}
	if cfg.MaxConns == 0 || cfg.MaxConns > 20 {
		cfg.MaxConns = 20
	}
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgdb: new pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgdb: ping: %w", err)
	}
	return pool, nil
}

// Ping 為 readiness 檢查用。
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return pool.Ping(ctx)
}

// newMigrator 建立 golang-migrate 實例（iofs source + pgx/v5 driver，內建 advisory lock）。
func newMigrator(url, service string, src fs.FS) (*migrate.Migrate, func(), error) {
	srcDriver, err := iofs.New(src, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("pgdb: migration source: %w", err)
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, nil, fmt.Errorf("pgdb: open: %w", err)
	}
	dbDriver, err := migratepgx.WithInstance(db, &migratepgx.Config{MigrationsTable: "schema_migrations"})
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("pgdb: migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx5/"+service, dbDriver)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("pgdb: migrator: %w", err)
	}
	cleanup := func() {
		_, _ = m.Close()
	}
	return m, cleanup, nil
}

// Migrate 套用所有尚未執行的 up migration（冪等：已是最新時不回錯誤）。
func Migrate(_ context.Context, url, service string, src fs.FS) error {
	m, cleanup, err := newMigrator(url, service, src)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("pgdb: migrate up (%s): %w", service, err)
	}
	return nil
}

// MigrateDown 回退 steps 步（steps ≤ 0 時回退 1 步）。
func MigrateDown(_ context.Context, url, service string, src fs.FS, steps int) error {
	if steps <= 0 {
		steps = 1
	}
	m, cleanup, err := newMigrator(url, service, src)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := m.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("pgdb: migrate down %d (%s): %w", steps, service, err)
	}
	return nil
}

// MigrateVersion 回傳目前版本與 dirty 旗標；尚未執行任何 migration 時 version=0。
func MigrateVersion(_ context.Context, url, service string, src fs.FS) (version uint, dirty bool, err error) {
	m, cleanup, err := newMigrator(url, service, src)
	if err != nil {
		return 0, false, err
	}
	defer cleanup()
	v, d, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("pgdb: migrate version (%s): %w", service, err)
	}
	return v, d, nil
}

// WithTx 在交易內執行 fn；fn 回錯誤或 panic 時 rollback，否則 commit。
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgdb: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			rollback(ctx, tx)
			panic(p)
		}
		if err != nil {
			rollback(ctx, tx)
		}
	}()
	err = fn(tx)
	if err != nil {
		return err
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		err = fmt.Errorf("pgdb: commit: %w", cerr)
		return err
	}
	return nil
}

// rollback 忽略 ErrTxClosed（commit 失敗後 tx 已關閉）。
func rollback(ctx context.Context, tx pgx.Tx) {
	if rerr := tx.Rollback(ctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
		slog.Default().Warn("pgdb: rollback failed", "err", rerr)
	}
}

// IsUniqueViolation 判斷是否為唯一鍵衝突（SQLSTATE 23505）。
func IsUniqueViolation(err error) bool { return hasSQLState(err, sqlstateUniqueViolation) }

// IsCheckViolation 判斷是否為 CHECK 約束違反（SQLSTATE 23514）。
func IsCheckViolation(err error) bool { return hasSQLState(err, sqlstateCheckViolation) }

// IsForeignKeyViolation 判斷是否為外鍵違反（SQLSTATE 23503）。
func IsForeignKeyViolation(err error) bool { return hasSQLState(err, sqlstateFKViolation) }

// ConstraintName 回傳違反的約束名稱（非 PgError 時為空）。
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// IsNoRows 判斷是否為 pgx.ErrNoRows 或 ErrNotFound。
func IsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrNotFound)
}

func hasSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// 確保 stdlib driver 有被註冊（database/sql "pgx"）。
var _ = stdlib.GetDefaultDriver

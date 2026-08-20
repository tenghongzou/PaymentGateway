package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TopicFunc 決定每筆訊息要送到哪個 topic。
type TopicFunc func(msg Message) string

// Batcher 抽象「取得一批未發佈訊息 → 處理 → 標記結果」的交易邊界，讓 Relay 迴圈可被單元測試。
type Batcher interface {
	// ProcessBatch 在一個交易內以 FOR UPDATE SKIP LOCKED 取最多 limit 筆，交給 fn；
	// fn 回傳與 msgs 等長的結果（nil = 已發佈），Batcher 據此更新 published_at 或 attempts/last_error。
	// 回傳本批筆數與失敗筆數。
	ProcessBatch(ctx context.Context, limit int, fn func(ctx context.Context, msgs []Message) []error) (total, failed int, err error)
}

// RelayConfig 為 Relay 設定。
type RelayConfig struct {
	Batcher      Batcher
	Publisher    Publisher
	Topic        TopicFunc
	BatchSize    int           // 預設 100
	PollInterval time.Duration // 無資料時的輪詢間隔，預設 500ms
	MaxBackoff   time.Duration // 連續失敗時的指數退避上限，預設 30s
	Logger       *slog.Logger
}

// Relay 為 outbox relay worker；每個服務實例都跑一個，靠 SKIP LOCKED 天然分片。
type Relay struct {
	cfg     RelayConfig
	backoff time.Duration
}

// NewRelay 建立 Relay。
func NewRelay(cfg RelayConfig) *Relay {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Relay{cfg: cfg}
}

// Run 持續輪詢直到 ctx 結束；結束前會完成當前批次（graceful stop）。
func (r *Relay) Run(ctx context.Context) error {
	log := r.cfg.Logger.With("component", "outbox-relay")
	log.Info("outbox relay started", "batch_size", r.cfg.BatchSize)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("outbox relay stopped")
			return nil
		case <-timer.C:
		}
		// 用 WithoutCancel 讓進行中的批次完成（上限 10s），避免 produce 成功但 UPDATE 被取消造成重送。
		batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		total, failed, err := r.cfg.Batcher.ProcessBatch(batchCtx, r.cfg.BatchSize, r.publish)
		cancel()
		timer.Reset(r.nextDelay(total, failed, err, log))
	}
}

// RunOnce 處理一個批次（測試與 CLI 用）。
func (r *Relay) RunOnce(ctx context.Context) (total, failed int, err error) {
	return r.cfg.Batcher.ProcessBatch(ctx, r.cfg.BatchSize, r.publish)
}

func (r *Relay) publish(ctx context.Context, msgs []Message) []error {
	results := make([]error, len(msgs))
	for i, m := range msgs {
		topic := r.cfg.Topic(m)
		headers := map[string]string{"event_id": m.ID, "event_type": m.EventType, "aggregate_type": m.AggregateType}
		for k, v := range m.Headers {
			headers[k] = v
		}
		if err := r.cfg.Publisher.Publish(ctx, topic, m.AggregateID, m.Payload, headers); err != nil {
			results[i] = err
		}
	}
	return results
}

// nextDelay 決定下一輪的等待：有錯誤 → 指數退避；批次滿 → 立即；否則 PollInterval。
func (r *Relay) nextDelay(total, failed int, err error, log *slog.Logger) time.Duration {
	if err != nil || failed > 0 {
		if r.backoff == 0 {
			r.backoff = r.cfg.PollInterval
		} else {
			r.backoff *= 2
		}
		if r.backoff > r.cfg.MaxBackoff {
			r.backoff = r.cfg.MaxBackoff
		}
		log.Warn("outbox relay batch had failures", "total", total, "failed", failed, "err", err, "backoff", r.backoff)
		return r.backoff
	}
	r.backoff = 0
	if total >= r.cfg.BatchSize {
		return 0
	}
	return r.cfg.PollInterval
}

// PGBatcher 為 Batcher 的 PostgreSQL 實作。
type PGBatcher struct {
	pool *pgxpool.Pool
}

// NewPGBatcher 建立 PGBatcher。
func NewPGBatcher(pool *pgxpool.Pool) *PGBatcher { return &PGBatcher{pool: pool} }

// ProcessBatch 實作 Batcher。
func (b *PGBatcher) ProcessBatch(ctx context.Context, limit int, fn func(ctx context.Context, msgs []Message) []error) (total, failed int, err error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("outbox: begin: %w", err)
	}
	defer rollbackQuietly(ctx, tx)

	msgs, err := fetchUnpublished(ctx, tx, limit)
	if err != nil {
		return 0, 0, err
	}
	if len(msgs) == 0 {
		return 0, 0, tx.Commit(ctx)
	}
	results := fn(ctx, msgs)
	for i, m := range msgs {
		if i < len(results) && results[i] != nil {
			failed++
			if _, uerr := tx.Exec(ctx, `UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1::uuid`,
				m.ID, truncate(results[i].Error(), 500)); uerr != nil {
				return len(msgs), failed, fmt.Errorf("outbox: mark failed: %w", uerr)
			}
			continue
		}
		if _, uerr := tx.Exec(ctx, `UPDATE outbox SET published_at = now(), attempts = attempts + 1, last_error = NULL WHERE id = $1::uuid`, m.ID); uerr != nil {
			return len(msgs), failed, fmt.Errorf("outbox: mark published: %w", uerr)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return len(msgs), failed, fmt.Errorf("outbox: commit: %w", err)
	}
	return len(msgs), failed, nil
}

func fetchUnpublished(ctx context.Context, tx pgx.Tx, limit int) ([]Message, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, aggregate_type, aggregate_id, event_type, payload, headers, attempts
		  FROM outbox
		 WHERE published_at IS NULL
		 ORDER BY created_at
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: fetch: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var headers []byte
		if err := rows.Scan(&m.ID, &m.AggregateType, &m.AggregateID, &m.EventType, &m.Payload, &headers, &m.Attempts); err != nil {
			return nil, fmt.Errorf("outbox: scan: %w", err)
		}
		if len(headers) > 0 {
			if err := json.Unmarshal(headers, &m.Headers); err != nil {
				return nil, fmt.Errorf("outbox: decode headers of %s: %w", m.ID, err)
			}
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: rows: %w", err)
	}
	return out, nil
}

// PendingLag 回傳最舊未發佈訊息的延遲秒數與筆數（給 pg_outbox_lag_seconds / pg_outbox_pending 指標）。
func PendingLag(ctx context.Context, pool *pgxpool.Pool) (lag time.Duration, pending int64, err error) {
	var oldest *time.Time
	err = pool.QueryRow(ctx, `SELECT min(created_at), count(*) FROM outbox WHERE published_at IS NULL`).Scan(&oldest, &pending)
	if err != nil {
		return 0, 0, fmt.Errorf("outbox: pending lag: %w", err)
	}
	if oldest != nil {
		lag = time.Since(*oldest)
	}
	return lag, pending, nil
}

// rollbackQuietly 在 commit 後呼叫 Rollback 會得到 ErrTxClosed，屬預期行為。
func rollbackQuietly(ctx context.Context, tx pgx.Tx) {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		slog.Default().Warn("outbox: rollback failed", "err", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ErrStopped 供呼叫端判斷 relay 正常結束。
var ErrStopped = errors.New("outbox: relay stopped")

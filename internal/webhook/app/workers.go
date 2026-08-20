package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
)

// Dispatcher 為投遞 worker：每 Interval 輪詢一次，取 Batch 筆並以 Concurrency 個 goroutine 送出；
// 若該輪取滿一批則不等待、立即再取（排空 backlog）。多副本靠 FOR UPDATE SKIP LOCKED 分工。
type Dispatcher struct {
	Svc         *Service
	Interval    time.Duration
	Batch       int
	Concurrency int
	Logger      *slog.Logger
}

// Run 執行到 ctx 結束。
func (w *Dispatcher) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	batch := w.Batch
	if batch <= 0 {
		batch = 50
	}
	conc := w.Concurrency
	if conc <= 0 {
		conc = 16
	}
	log := w.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("worker", "webhook-dispatcher")
	log.Info("dispatcher started", "interval", interval, "batch", batch, "concurrency", conc)

	timer := time.NewTimer(0)
	defer timer.Stop()
	errBackoff := interval
	for {
		select {
		case <-ctx.Done():
			log.Info("dispatcher stopped")
			return nil
		case <-timer.C:
		}
		n, err := w.Svc.DispatchDue(ctx, batch, conc)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			log.Error("dispatch failed", "err", err)
			errBackoff = min(errBackoff*2, 10*time.Second)
			timer.Reset(errBackoff)
		case n >= batch:
			errBackoff = interval
			timer.Reset(0)
		default:
			errBackoff = interval
			timer.Reset(interval)
		}
	}
}

// Reaper 定期把卡住的 in_flight delivery 轉回 failed。
type Reaper struct {
	Svc      *Service
	Interval time.Duration
	Timeout  time.Duration
	Logger   *slog.Logger
}

// Run 執行到 ctx 結束。
func (w *Reaper) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = domain.InFlightTimeout
	}
	log := w.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("worker", "webhook-reaper")
	log.Info("reaper started", "interval", interval, "in_flight_timeout", timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("reaper stopped")
			return nil
		case <-ticker.C:
		}
		if _, err := w.Svc.ReapStuckInFlight(ctx, timeout); err != nil && ctx.Err() == nil {
			log.Error("reap failed", "err", err)
		}
	}
}

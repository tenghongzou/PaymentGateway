// Package devsink 為本機開發用的 webhook 接收端：印出收到的 webhook、以 pkg/sig.VerifyWebhook 驗簽，
// 並可指定固定回應碼模擬商戶端失敗（測試重試 / 410 停用）。
//
// 用法：`webhook-service sink -addr :8099 -secret whsec_xxx [-secret whsec_old] [-status 500]`
package devsink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tenghongzou/paymentgateway/pkg/sig"
)

// Options 為 sink 設定。
type Options struct {
	// Secrets 為驗簽 secret；空表示不驗簽（只印出並警告）。
	Secrets []string
	// Status 為固定回應碼（預設 200）。
	Status int
	// FailFirst 讓前 N 次請求回 FailStatus（預設 0 = 不模擬），之後回 Status；用來觀察重試。
	FailFirst  int
	FailStatus int
	// Tolerance 為簽章時間窗（預設 300s）。
	Tolerance time.Duration
	// Out 為輸出（預設 stdout）。
	Out io.Writer
	Log *slog.Logger
}

// Handler 回傳接收 webhook 的 http.Handler。
func Handler(o Options) http.Handler {
	if o.Status == 0 {
		o.Status = http.StatusOK
	}
	if o.FailStatus == 0 {
		o.FailStatus = http.StatusInternalServerError
	}
	if o.Tolerance <= 0 {
		o.Tolerance = sig.DefaultWindow
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	var count atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		n := count.Add(1)
		header := r.Header.Get("X-PG-Signature")
		verified := "skipped (no secret configured)"
		if len(o.Secrets) > 0 {
			if err := sig.VerifyWebhook(o.Secrets, header, body, time.Now(), o.Tolerance); err != nil {
				verified = "FAILED: " + err.Error()
				printEvent(o.Out, n, r, body, verified)
				http.Error(w, "invalid signature", http.StatusBadRequest)
				return
			}
			verified = "ok"
		}
		printEvent(o.Out, n, r, body, verified)
		status := o.Status
		if o.FailFirst > 0 && n <= int64(o.FailFirst) {
			status = o.FailStatus
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"received":true,"status":%d}`, status)
	})
}

func printEvent(out io.Writer, n int64, r *http.Request, body []byte, verified string) {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "  ", "  "); err != nil {
		pretty.Reset()
		pretty.Write(body)
	}
	fmt.Fprintf(out, "\n=== webhook #%d %s ===\n", n, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "  %s %s\n", r.Method, r.URL.Path)
	for _, h := range []string{"X-PG-Event-Id", "X-PG-Event-Type", "X-PG-Delivery-Id", "X-PG-Attempt", "X-PG-Signature", "User-Agent", "Content-Type"} {
		if v := r.Header.Get(h); v != "" {
			fmt.Fprintf(out, "  %s: %s\n", h, v)
		}
	}
	fmt.Fprintf(out, "  signature: %s\n  body:\n  %s\n", verified, pretty.String())
}

// Main 為 `webhook-service sink` 子命令進入點。
func Main(args []string) int {
	fs := flag.NewFlagSet("sink", flag.ContinueOnError)
	addr := fs.String("addr", ":8099", "listen address")
	status := fs.Int("status", 200, "response status code")
	failFirst := fs.Int("fail-first", 0, "respond with -fail-status for the first N requests (simulate retries)")
	failStatus := fs.Int("fail-status", 500, "status code used by -fail-first")
	var secrets multiFlag
	fs.Var(&secrets, "secret", "webhook signing secret (repeatable; current + previous)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if env := os.Getenv("PG_WEBHOOK_DEV_ENDPOINT_SECRET"); env != "" && len(secrets) == 0 {
		secrets = append(secrets, env)
	}
	log := slog.Default()
	srv := &http.Server{Addr: *addr, Handler: Handler(Options{Secrets: secrets, Status: *status, FailFirst: *failFirst, FailStatus: *failStatus, Log: log}), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("webhook dev sink listening", "addr", *addr, "secrets", len(secrets), "status", *status, "fail_first", *failFirst)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("sink failed", "err", err)
		return 1
	}
	return 0
}

type multiFlag []string

func (m *multiFlag) String() string { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// Package logx 建構 slog JSON logger，並提供 trace / request id 注入與敏感資料遮罩。
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
)

// New 建立 JSON logger，附帶 service / env 欄位。
func New(service, env, level string) *slog.Logger {
	return NewWithWriter(os.Stdout, service, env, level)
}

// NewWithWriter 建立寫到指定 writer 的 JSON logger（測試用）。
func NewWithWriter(w io.Writer, service, env, level string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(level)})
	return slog.New(h).With("service", service, "env", env)
}

// ParseLevel 把字串轉成 slog.Level，未知值回 Info。
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// IntoContext 把 logger 放進 context。
func IntoContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext 取出 logger 並自動附上 trace_id / span_id / request_id；沒有時回傳 slog.Default()。
func FromContext(ctx context.Context) *slog.Logger {
	l, ok := ctx.Value(loggerKey).(*slog.Logger)
	if !ok || l == nil {
		l = slog.Default()
	}
	return WithTrace(ctx, l)
}

// WithTrace 從 context 取 OpenTelemetry span 與 request id，注入 log 欄位。
func WithTrace(ctx context.Context, l *slog.Logger) *slog.Logger {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		l = l.With("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	if rid := RequestIDFromContext(ctx); rid != "" {
		l = l.With("request_id", rid)
	}
	return l
}

// WithRequestID 把 request id 放進 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext 取出 request id（沒有時為空字串）。
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// TraceIDFromContext 取出目前 span 的 trace id（沒有時為空字串）。
func TraceIDFromContext(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// MaskPAN 遮罩卡號：只保留前 6 後 4（長度不足 10 則全部遮罩）。本系統不應持有 PAN，此函式只作為防呆。
func MaskPAN(pan string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, pan)
	if len(digits) < 10 {
		return strings.Repeat("*", len(digits))
	}
	return digits[:6] + strings.Repeat("*", len(digits)-10) + digits[len(digits)-4:]
}

// MaskSecret 遮罩祕密（API key、signing secret、token）：保留前 8 碼（含前綴）與後 4 碼，其餘以 * 取代；
// 長度 ≤ 12 時只顯示前 4 碼。
func MaskSecret(s string) string {
	switch {
	case s == "":
		return ""
	case len(s) <= 4:
		return strings.Repeat("*", len(s))
	case len(s) <= 12:
		return s[:4] + strings.Repeat("*", len(s)-4)
	default:
		return s[:8] + "…" + s[len(s)-4:]
	}
}

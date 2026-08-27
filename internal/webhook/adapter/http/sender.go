// Package http 實作對商戶端點的 HTTP 投遞（app.HTTPSender）。
//
// 安全措施（docs/06 §4.5）：自訂 Transport 在 DialContext 層重新解析 DNS 並檢查 IP（避免 DNS rebinding），
// 固定以檢查過的 IP 連線；不跟隨 redirect；不讀取環境 proxy；回應 body 只讀前 4KB。
package http

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
)

// 預設逾時（docs/06 §4.4：連線 3s、整體 10s）。
const (
	DefaultTimeout        = 10 * time.Second
	DefaultConnectTimeout = 3 * time.Second
)

// Options 為 Sender 選項。
type Options struct {
	// Policy 為 SSRF 政策（預設 StrictPolicy）。
	Policy domain.URLPolicy
	// Timeout 為整體逾時（預設 10s）。
	Timeout time.Duration
	// ConnectTimeout 為 TCP 連線逾時（預設 3s）。
	ConnectTimeout time.Duration
	// Resolver 可注入（測試用）。
	Resolver domain.Resolver
	// TLSConfig 可注入（測試用自簽憑證）。
	TLSConfig *tls.Config
	// MaxBodyBytes 為記錄的回應 body 上限（預設 domain.MaxResponseBodyBytes）。
	MaxBodyBytes int
}

// Sender 實作 app.HTTPSender。
type Sender struct {
	client   *http.Client
	policy   domain.URLPolicy
	maxBody  int
	timeout  time.Duration
	resolver domain.Resolver
}

// NewSender 建立 Sender。
func NewSender(o Options) *Sender {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = DefaultConnectTimeout
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = domain.MaxResponseBodyBytes
	}
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	s := &Sender{policy: o.Policy, maxBody: o.MaxBodyBytes, timeout: o.Timeout, resolver: o.Resolver}
	dialer := &net.Dialer{Timeout: o.ConnectTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil, // 不讀 HTTP_PROXY：避免經由 proxy 繞過 IP 檢查
		DialContext:           s.dialContext(dialer),
		TLSClientConfig:       o.TLSConfig,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: o.Timeout,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else if tr.TLSClientConfig.MinVersion == 0 {
		tr.TLSClientConfig.MinVersion = tls.VersionTLS12
	}
	s.client = &http.Client{
		Transport: tr,
		Timeout:   o.Timeout,
		// 3xx 不跟隨、視為失敗（docs/06 §4.4）。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return s
}

// dialContext 在連線層再做一次 DNS 解析 + IP 檢查，並以檢查過的 IP 連線。
func (s *Sender) dialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		addrs, err := s.policy.ResolveAndCheck(ctx, s.resolver, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, a := range addrs {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("webhook: no reachable address for %s", host)
		}
		return nil, lastErr
	}
}

// Send 實作 app.HTTPSender。
func (s *Sender) Send(ctx context.Context, req app.SendRequest) domain.Outcome {
	start := time.Now()
	done := func(o domain.Outcome) domain.Outcome {
		o.Duration = time.Since(start)
		return o
	}
	if _, err := s.policy.ValidateURL(req.URL); err != nil {
		return done(domain.Outcome{Err: err})
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, strings.NewReader(string(req.Body)))
	if err != nil {
		return done(domain.Outcome{Err: fmt.Errorf("webhook: build request: %w", err)})
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", domain.ContentTypeJSON)
	}
	if httpReq.Header.Get("User-Agent") == "" {
		httpReq.Header.Set("User-Agent", domain.UserAgent)
	}
	httpReq.ContentLength = int64(len(req.Body))

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return done(domain.Outcome{Err: classifyErr(err)})
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(s.maxBody)))
	// 把剩餘 body 丟掉（上限 64KB）讓連線可重用。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) //nolint:errcheck // 盡力 drain 供連線重用；失敗只影響連線重用，無需處理
	out := domain.Outcome{StatusCode: resp.StatusCode, Body: string(body)}
	if readErr != nil {
		// 狀態碼已取得（結果分類不受影響），把讀取中斷記進 Outcome.Err 供診斷。
		out.Err = fmt.Errorf("read response body: %w", classifyErr(readErr))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		out.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	return done(out)
}

// classifyErr 把 client 錯誤整理成可讀訊息（避免把整個 URL 重複放進 last_error）。
func classifyErr(err error) error {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("request timed out")
	case errors.As(err, &ne) && ne.Timeout():
		return errors.New("request timed out: " + unwrapURLErr(err))
	case errors.Is(err, domain.ErrIPNotAllowed), errors.Is(err, domain.ErrURLNotAllowed):
		return err
	default:
		return errors.New(unwrapURLErr(err))
	}
}

func unwrapURLErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// parseRetryAfter 解析 Retry-After（秒數或 HTTP-date）；解析失敗回 0。
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// CheckAddrs 供外部（例如端點建立時）重用同一套 IP 檢查。
func (s *Sender) CheckAddrs(addrs []netip.Addr) error {
	for _, a := range addrs {
		if err := s.policy.CheckAddr(a); err != nil {
			return err
		}
	}
	return nil
}

var _ app.HTTPSender = (*Sender)(nil)

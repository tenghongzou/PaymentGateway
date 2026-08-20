package grpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
)

// Fetcher 依 source_url 取得結算檔內容。
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) ([]byte, error)
}

// FetcherOptions 為預設 Fetcher 的選項。
type FetcherOptions struct {
	// MaxBytes 為下載上限（超過回 RESOURCE_EXHAUSTED 等級錯誤）。
	MaxBytes int64
	// HTTPClient 為 http(s) 下載用 client；nil → 60s timeout 的預設 client。
	HTTPClient *http.Client
	// AllowedDirs 限制 file:// 只能讀取這些目錄底下的檔案（空 → 不限制；生產環境務必設定）。
	AllowedDirs []string
}

// ErrFileTooLarge 表示超過大小上限（grpcx 依 HTTP 429 對應到 RESOURCE_EXHAUSTED）。
var ErrFileTooLarge = apperr.New(apperr.TypeRateLimit, "rate_limit_exceeded", "Settlement file exceeds the size limit.").WithParam("source_url")

// defaultFetcher 支援 file:// 與 http(s)://。
//
// TODO(phase-1)：s3:// / sftp:// 來源（proto 註解要求 s3），並把原檔存到物件儲存（settlement_files.storage_uri）。
type defaultFetcher struct {
	opts FetcherOptions
}

// NewFetcher 建立預設 Fetcher。
func NewFetcher(opts FetcherOptions) Fetcher {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxFileBytes
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &defaultFetcher{opts: opts}
}

// Fetch 實作 Fetcher。
func (f *defaultFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, apperr.ErrParameterInvalid.WithMessage("source_url is not a valid URL.").WithParam("source_url")
	}
	switch strings.ToLower(u.Scheme) {
	case "file":
		return f.fetchFile(u)
	case "http", "https":
		return f.fetchHTTP(ctx, rawURL)
	default:
		return nil, apperr.ErrParameterInvalid.WithMessage("source_url scheme %q is not supported (file://, http://, https://).", u.Scheme).WithParam("source_url")
	}
}

func (f *defaultFetcher) fetchFile(u *url.URL) ([]byte, error) {
	path := u.Path
	if u.Host != "" && u.Host != "localhost" {
		// file://host/path 非本機：拒絕。
		return nil, apperr.ErrParameterInvalid.WithMessage("file:// URL must not specify a host.").WithParam("source_url")
	}
	abs, err := filepath.Abs(filepath.FromSlash(path))
	if err != nil {
		return nil, apperr.ErrParameterInvalid.WithMessage("source_url path is invalid.").WithParam("source_url")
	}
	if len(f.opts.AllowedDirs) > 0 && !underAny(abs, f.opts.AllowedDirs) {
		return nil, apperr.New(apperr.TypeAuthentication, "insufficient_permissions", "source_url path is outside the allowed directories.").WithParam("source_url")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, apperr.ErrResourceMissing.WithMessage("source_url file not found: %v", err).WithParam("source_url")
	}
	if info.IsDir() {
		return nil, apperr.ErrParameterInvalid.WithMessage("source_url points to a directory.").WithParam("source_url")
	}
	if info.Size() > f.opts.MaxBytes {
		return nil, ErrFileTooLarge
	}
	fh, err := os.Open(abs)
	if err != nil {
		return nil, apperr.ErrResourceMissing.WithMessage("source_url file cannot be opened: %v", err).WithParam("source_url")
	}
	defer fh.Close()
	return f.readLimited(fh)
}

func (f *defaultFetcher) fetchHTTP(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, apperr.ErrParameterInvalid.WithMessage("source_url is invalid: %v", err).WithParam("source_url")
	}
	resp, err := f.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, apperr.ErrServiceUnavailable.WithMessage("download source_url failed: %v", err).Wrap(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apperr.ErrParameterInvalid.WithMessage("download source_url failed: HTTP %d.", resp.StatusCode).WithParam("source_url")
	}
	if resp.ContentLength > f.opts.MaxBytes {
		return nil, ErrFileTooLarge
	}
	return f.readLimited(resp.Body)
}

// readLimited 讀取最多 MaxBytes+1 個位元組，超過即視為過大。
func (f *defaultFetcher) readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, f.opts.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}
	if int64(len(b)) > f.opts.MaxBytes {
		return nil, ErrFileTooLarge
	}
	return b, nil
}

func underAny(path string, dirs []string) bool {
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(abs, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

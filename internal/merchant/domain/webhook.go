package domain

import (
	"context"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tenghongzou/paymentgateway/pkg/ids"
)

// EndpointStatus 為 webhook_endpoints.status（DB CHECK 只允許 enabled / disabled）。
type EndpointStatus string

// EndpointStatus 全集。
const (
	EndpointEnabled  EndpointStatus = "enabled"
	EndpointDisabled EndpointStatus = "disabled"
)

// SecretRotationGrace 為輪替後舊 secret 仍有效的時間（proto：24h；docs/06 §4.1 上限 72h）。
const SecretRotationGrace = 24 * time.Hour

// MaxEndpointDescriptionLen 為說明長度上限。
const MaxEndpointDescriptionLen = 200

// WebhookEventWildcard 表示訂閱全部事件。
const WebhookEventWildcard = "*"

// knownWebhookEvents 為可訂閱的事件型別（docs/03 §5.1 + docs/02 附錄 B）。
var knownWebhookEvents = map[string]struct{}{
	"payment.created": {}, "payment.requires_action": {}, "payment.authorized": {}, "payment.captured": {},
	"payment.voided": {}, "payment.failed": {}, "payment.expired": {},
	"refund.created": {}, "refund.succeeded": {}, "refund.failed": {},
	"dispute.opened": {}, "dispute.evidence_submitted": {}, "dispute.evidence_due_soon": {},
	"dispute.won": {}, "dispute.lost": {},
	"balance.updated": {},
}

// ValidateEnabledEvents 檢查事件型別；接受 "*"、完整名稱或 "<resource>.*"。回傳正規化（去重、空 → {"*"}）後的清單。
func ValidateEnabledEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return []string{WebhookEventWildcard}, nil
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		e = strings.TrimSpace(e)
		if e == WebhookEventWildcard {
			return []string{WebhookEventWildcard}, nil
		}
		if !isKnownWebhookEvent(e) {
			return nil, invalid("enabled_events", "unknown event type %q", e)
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func isKnownWebhookEvent(e string) bool {
	if _, ok := knownWebhookEvents[e]; ok {
		return true
	}
	if res, ok := strings.CutSuffix(e, ".*"); ok && res != "" {
		for k := range knownWebhookEvents {
			if strings.HasPrefix(k, res+".") {
				return true
			}
		}
	}
	return false
}

// WebhookEndpoint 為商戶接收事件通知的端點（docs/02 §2.1）。
//
// DB 沒有 mode / deleted_at / auto_disabled 專欄，Phase 0 放在 metadata jsonb 的內部鍵（_mode、_deleted_at、_auto_disabled）；
// 軟刪除以 status=disabled + DeletedAt 表示。TODO：補 migration 加專欄並擴充 status CHECK。
type WebhookEndpoint struct {
	ID          uuid.UUID
	MerchantID  uuid.UUID
	URL         string
	Description string
	// SecretCurrentEnc / SecretPreviousEnc 為 SecretCipher 加密後的 whsec_ secret。
	SecretCurrentEnc  string
	SecretPreviousEnc string
	SecretRotatedAt   *time.Time
	EnabledEvents     []string
	Status            EndpointStatus
	Mode              Mode
	AutoDisabled      bool
	DeletedAt         *time.Time
	Metadata          map[string]string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Version           int
}

// PublicID 回傳對外 ID（we_ + base32(uuid)）。
func (e *WebhookEndpoint) PublicID() string { return ids.Format(ids.PrefixWebhookEndpoint, e.ID) }

// ParseWebhookEndpointID 解析 we_... 為 uuid。
func ParseWebhookEndpointID(publicID string) (uuid.UUID, error) {
	if publicID == "" {
		return uuid.Nil, missing("endpoint_id")
	}
	u, err := ids.ParseWithPrefix(publicID, ids.PrefixWebhookEndpoint)
	if err != nil {
		return uuid.Nil, invalid("endpoint_id", "endpoint_id must look like we_<26 chars>")
	}
	return u, nil
}

// NewWebhookEndpointParams 為建立端點的輸入。
type NewWebhookEndpointParams struct {
	MerchantID    uuid.UUID
	URL           string
	Description   string
	EnabledEvents []string
	Mode          Mode
	Metadata      map[string]string
	// SecretEnc 為已加密的 whsec_ secret（由 app 層產生明文並加密）。
	SecretEnc string
}

// NewWebhookEndpoint 建立端點（URL 需先經 URLPolicy.Validate）。
func NewWebhookEndpoint(p NewWebhookEndpointParams, now time.Time) (*WebhookEndpoint, error) {
	if _, err := ParseMode(string(p.Mode)); err != nil {
		return nil, err
	}
	events, err := ValidateEnabledEvents(p.EnabledEvents)
	if err != nil {
		return nil, err
	}
	if err := ValidateEndpointDescription(p.Description); err != nil {
		return nil, err
	}
	if err := ValidateMetadata(p.Metadata); err != nil {
		return nil, err
	}
	if p.SecretEnc == "" {
		return nil, missing("secret")
	}
	return &WebhookEndpoint{
		ID:               ids.NewUUID(),
		MerchantID:       p.MerchantID,
		URL:              p.URL,
		Description:      p.Description,
		SecretCurrentEnc: p.SecretEnc,
		EnabledEvents:    events,
		Status:           EndpointEnabled,
		Mode:             p.Mode,
		Metadata:         cloneMetadata(p.Metadata),
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// ValidateEndpointDescription 檢查說明長度。
func ValidateEndpointDescription(d string) error {
	if len([]rune(d)) > MaxEndpointDescriptionLen {
		return invalid("description", "description must be at most %d characters", MaxEndpointDescriptionLen)
	}
	return nil
}

// IsDeleted 回傳是否已軟刪除。
func (e *WebhookEndpoint) IsDeleted() bool { return e.DeletedAt != nil }

// PreviousSecretExpiresAt 回傳上一把 secret 的失效時間（secret_rotated_at + grace）。
func (e *WebhookEndpoint) PreviousSecretExpiresAt() *time.Time {
	if e.SecretPreviousEnc == "" || e.SecretRotatedAt == nil {
		return nil
	}
	t := e.SecretRotatedAt.Add(SecretRotationGrace)
	return &t
}

// PreviousSecretValid 回傳上一把 secret 是否仍在輪替視窗內。
func (e *WebhookEndpoint) PreviousSecretValid(now time.Time) bool {
	exp := e.PreviousSecretExpiresAt()
	return exp != nil && exp.After(now)
}

// RotateSecret 輪替：current → previous，newEnc 成為 current。
func (e *WebhookEndpoint) RotateSecret(newEnc string, now time.Time) {
	e.SecretPreviousEnc = e.SecretCurrentEnc
	e.SecretCurrentEnc = newEnc
	t := now
	e.SecretRotatedAt = &t
}

// ExpirePreviousSecret 若視窗已過則清除 previous；回傳是否有變更。
func (e *WebhookEndpoint) ExpirePreviousSecret(now time.Time) bool {
	if e.SecretPreviousEnc != "" && !e.PreviousSecretValid(now) {
		e.SecretPreviousEnc = ""
		return true
	}
	return false
}

// SetURL 更新 URL（需先經 URLPolicy.Validate）。
func (e *WebhookEndpoint) SetURL(u string) { e.URL = u }

// SetDescription 更新說明。
func (e *WebhookEndpoint) SetDescription(d string) error {
	if err := ValidateEndpointDescription(d); err != nil {
		return err
	}
	e.Description = d
	return nil
}

// SetEnabledEvents 更新訂閱事件。
func (e *WebhookEndpoint) SetEnabledEvents(events []string) error {
	out, err := ValidateEnabledEvents(events)
	if err != nil {
		return err
	}
	e.EnabledEvents = out
	return nil
}

// SetMetadata 整份取代 metadata。
func (e *WebhookEndpoint) SetMetadata(md map[string]string) error {
	if err := ValidateMetadata(md); err != nil {
		return err
	}
	e.Metadata = cloneMetadata(md)
	return nil
}

// Enable 啟用（同時清除 auto_disabled 標記）。
func (e *WebhookEndpoint) Enable() error {
	if e.IsDeleted() {
		return ErrInvalidStateTransition.WithParam("status").WithMessage("endpoint is deleted")
	}
	e.Status = EndpointEnabled
	e.AutoDisabled = false
	return nil
}

// Disable 手動停用。
func (e *WebhookEndpoint) Disable() error {
	if e.IsDeleted() {
		return ErrInvalidStateTransition.WithParam("status").WithMessage("endpoint is deleted")
	}
	e.Status = EndpointDisabled
	e.AutoDisabled = false
	return nil
}

// AutoDisable 由系統停用（連續失敗）。
func (e *WebhookEndpoint) AutoDisable() {
	e.Status = EndpointDisabled
	e.AutoDisabled = true
}

// MarkDeleted 軟刪除（冪等；回傳是否有變更）。
func (e *WebhookEndpoint) MarkDeleted(now time.Time) bool {
	if e.IsDeleted() {
		return false
	}
	t := now
	e.DeletedAt = &t
	e.Status = EndpointDisabled
	return true
}

// ---- URL 驗證（docs/06 §4.5 SSRF 防護）----

// Resolver 為 DNS 解析函式（可注入以利測試；nil 表示不解析）。
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// URLPolicy 定義端點 URL 的安全規則。
type URLPolicy struct {
	// AllowInsecure 為 true（dev）時允許 http、localhost / 私有 IP、任意 port。
	AllowInsecure bool
	// Resolver 非 nil 時會解析主機名並檢查所有 IP 不落在私有網段。
	Resolver Resolver
}

// 生產允許的 port。
var allowedPorts = map[string]struct{}{"": {}, "443": {}, "8443": {}}

// Validate 檢查 URL；違規回 ErrWebhookURLInvalid（帶說明）。
func (p URLPolicy) Validate(ctx context.Context, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return missing("url")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return ErrWebhookURLInvalid.WithParam("url").WithMessage("url must be an absolute URL")
	}
	if u.User != nil {
		return ErrWebhookURLInvalid.WithParam("url").WithMessage("url must not contain credentials")
	}
	if u.Fragment != "" {
		return ErrWebhookURLInvalid.WithParam("url").WithMessage("url must not contain a fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowInsecure {
			return ErrWebhookURLInvalid.WithParam("url").WithMessage("url must use https")
		}
	default:
		return ErrWebhookURLInvalid.WithParam("url").WithMessage("url scheme must be https")
	}
	if p.AllowInsecure {
		return nil
	}
	if _, ok := allowedPorts[u.Port()]; !ok {
		return ErrWebhookURLInvalid.WithParam("url").WithMessage("url port must be 443 or 8443")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil {
		return ErrWebhookURLInvalid.WithParam("url").WithMessage("url must use a hostname, not an IP literal")
	}
	if isForbiddenHostname(host) {
		return ErrWebhookURLInvalid.WithParam("url").WithMessage("url host %q is not allowed", host)
	}
	if p.Resolver != nil {
		addrs, err := p.Resolver(ctx, host)
		if err != nil || len(addrs) == 0 {
			return ErrWebhookURLInvalid.WithParam("url").WithMessage("url host %q could not be resolved", host)
		}
		for _, ip := range addrs {
			if IsPrivateIP(ip) {
				return ErrWebhookURLInvalid.WithParam("url").WithMessage("url host %q resolves to a private address", host)
			}
		}
	}
	return nil
}

// isForbiddenHostname 拒絕 localhost、.internal、.local、.localhost 與單一標籤主機名。
func isForbiddenHostname(host string) bool {
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".localhost", ".internal", ".local"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// cgnat 為 100.64.0.0/10（RFC 6598）。
var cgnat = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// IsPrivateIP 判斷 IP 是否為 loopback / RFC1918 / link-local（含 169.254.169.254 metadata）/ CGNAT / ULA / unspecified / multicast。
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && cgnat.Contains(v4) {
		return true
	}
	return false
}

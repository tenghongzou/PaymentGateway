package domain

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// URLPolicy 為端點 URL 的 SSRF 政策（docs/06 §4.5）。
//
// 嚴格模式（生產 / staging）：只允許 https、port 443 或 8443；拒絕 IP literal、localhost、*.internal、*.local；
// 解析後的 IP 不得為 loopback / private / link-local / CGNAT / metadata。
// AllowInsecure（dev）：允許 http、任意 port、IP literal 與私有位址，方便本機以 devsink 測試。
type URLPolicy struct {
	AllowInsecure bool
}

// StrictPolicy 為生產用政策。
var StrictPolicy = URLPolicy{}

// DevPolicy 為本機開發用政策。
var DevPolicy = URLPolicy{AllowInsecure: true}

// ValidateURL 檢查 scheme / host / port；回傳解析後的 URL。
func (p URLPolicy) ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrURLNotAllowed, err)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: userinfo not allowed", ErrURLNotAllowed)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: missing host", ErrURLNotAllowed)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowInsecure {
			return nil, fmt.Errorf("%w: scheme %q (https required)", ErrURLNotAllowed, u.Scheme)
		}
	default:
		return nil, fmt.Errorf("%w: scheme %q", ErrURLNotAllowed, u.Scheme)
	}
	if p.AllowInsecure {
		return u, nil
	}
	if port := u.Port(); port != "" && port != "443" && port != "8443" {
		return nil, fmt.Errorf("%w: port %s", ErrURLNotAllowed, port)
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if ip, err := netip.ParseAddr(lower); err == nil {
		return nil, fmt.Errorf("%w: ip literal %s", ErrURLNotAllowed, ip)
	}
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".internal") || strings.HasSuffix(lower, ".local") ||
		!strings.Contains(lower, ".") {
		return nil, fmt.Errorf("%w: host %q", ErrURLNotAllowed, host)
	}
	return u, nil
}

// 禁止的目的位址範圍。
var blockedPrefixes = func() []netip.Prefix {
	cidrs := []string{
		"0.0.0.0/8",       // this network
		"10.0.0.0/8",      // RFC1918
		"100.64.0.0/10",   // CGNAT
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local（含 169.254.169.254 metadata）
		"172.16.0.0/12",   // RFC1918
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"192.168.0.0/16",  // RFC1918
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved + broadcast
		"::/128",          // unspecified
		"::1/128",         // loopback
		"::ffff:0:0/96",   // IPv4-mapped（另會 unmap 後再檢查）
		"64:ff9b::/96",    // NAT64
		"fc00::/7",        // ULA
		"fe80::/10",       // link-local
		"ff00::/8",        // multicast
		"2001:db8::/32",   // documentation
	}
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}()

// CheckAddr 檢查單一 IP 是否允許連線。
func (p URLPolicy) CheckAddr(addr netip.Addr) error {
	if p.AllowInsecure {
		return nil
	}
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrIPNotAllowed)
	}
	a := addr.Unmap()
	if a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return fmt.Errorf("%w: %s", ErrIPNotAllowed, addr)
	}
	for _, pfx := range blockedPrefixes {
		if pfx.Contains(a) {
			return fmt.Errorf("%w: %s in %s", ErrIPNotAllowed, addr, pfx)
		}
	}
	return nil
}

// CheckIP 為 net.IP 版本的 CheckAddr。
func (p URLPolicy) CheckIP(ip net.IP) error {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("%w: invalid ip %v", ErrIPNotAllowed, ip)
	}
	return p.CheckAddr(addr)
}

// Resolver 為可注入的 DNS 解析器（預設 net.DefaultResolver）。
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ResolveAndCheck 解析 host 並逐一檢查；任一 IP 被禁止即拒絕（避免混入內網位址的 DNS 回應）。
// host 本身為 IP literal 時直接檢查。回傳通過檢查的位址清單供連線時固定使用（防 DNS rebinding）。
func (p URLPolicy) ResolveAndCheck(ctx context.Context, r Resolver, host string) ([]netip.Addr, error) {
	if r == nil {
		r = net.DefaultResolver
	}
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if err := p.CheckAddr(addr); err != nil {
			return nil, err
		}
		return []netip.Addr{addr}, nil
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("webhook: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("webhook: resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if err := p.CheckAddr(a); err != nil {
			return nil, err
		}
	}
	return addrs, nil
}

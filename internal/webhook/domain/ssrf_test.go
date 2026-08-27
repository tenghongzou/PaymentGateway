package domain

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestURLPolicyValidateURL(t *testing.T) {
	strict := StrictPolicy
	ok := []string{"https://merchant.example.com/hooks", "https://merchant.example.com:8443/hooks", "https://a.b.co:443/x?y=1"}
	for _, u := range ok {
		_, err := strict.ValidateURL(u)
		require.NoError(t, err, u)
	}
	bad := []string{
		"http://merchant.example.com/hooks",
		"https://merchant.example.com:8080/hooks",
		"https://127.0.0.1/hooks",
		"https://[::1]/hooks",
		"https://localhost/hooks",
		"https://api.localhost/hooks",
		"https://db.internal/hooks",
		"https://printer.local/hooks",
		"https://intranet/hooks",
		"https://user:pw@merchant.example.com/hooks",
		"ftp://merchant.example.com/hooks",
		"not a url",
		"",
	}
	for _, u := range bad {
		_, err := strict.ValidateURL(u)
		require.ErrorIs(t, err, ErrURLNotAllowed, u)
	}
	// dev 模式允許 http / localhost / 任意 port。
	for _, u := range []string{"http://localhost:8099/sink", "http://127.0.0.1:8099/sink", "https://merchant.example.com/hooks"} {
		_, err := DevPolicy.ValidateURL(u)
		require.NoError(t, err, u)
	}
	_, err := DevPolicy.ValidateURL("ftp://x/y")
	assert.Error(t, err)
}

func TestURLPolicyCheckAddr(t *testing.T) {
	strict := StrictPolicy
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "224.0.0.1", "255.255.255.255", "::1", "::", "fe80::1", "fd00::1", "::ffff:10.0.0.1", "64:ff9b::a00:1",
	}
	for _, s := range blocked {
		require.ErrorIs(t, strict.CheckAddr(netip.MustParseAddr(s)), ErrIPNotAllowed, s)
	}
	allowed := []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946", "172.32.0.1"}
	for _, s := range allowed {
		require.NoError(t, strict.CheckAddr(netip.MustParseAddr(s)), s)
	}
	assert.NoError(t, DevPolicy.CheckAddr(netip.MustParseAddr("127.0.0.1")))
}

type fakeResolver struct{ addrs map[string][]netip.Addr }

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return f.addrs[host], nil
}

func TestURLPolicyResolveAndCheck(t *testing.T) {
	r := fakeResolver{addrs: map[string][]netip.Addr{
		"good.example.com":  {netip.MustParseAddr("93.184.216.34")},
		"mixed.example.com": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.5")}, // DNS rebinding 手法
	}}
	addrs, err := StrictPolicy.ResolveAndCheck(context.Background(), r, "good.example.com")
	require.NoError(t, err)
	assert.Len(t, addrs, 1)
	_, err = StrictPolicy.ResolveAndCheck(context.Background(), r, "mixed.example.com")
	require.ErrorIs(t, err, ErrIPNotAllowed)
	_, err = StrictPolicy.ResolveAndCheck(context.Background(), r, "169.254.169.254")
	require.ErrorIs(t, err, ErrIPNotAllowed)
	_, err = StrictPolicy.ResolveAndCheck(context.Background(), r, "unknown.example.com")
	assert.Error(t, err)
}

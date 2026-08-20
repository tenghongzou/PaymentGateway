package domain

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestURLPolicyProduction(t *testing.T) {
	publicIP := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	privateIP := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.5")}, nil }
	metadataIP := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("169.254.169.254")}, nil }
	nxdomain := func(context.Context, string) ([]net.IP, error) { return nil, errors.New("no such host") }

	tests := []struct {
		name     string
		url      string
		resolver Resolver
		wantErr  bool
	}{
		{"https ok", "https://hooks.example.com/pg", publicIP, false},
		{"https 8443 ok", "https://hooks.example.com:8443/pg", publicIP, false},
		{"https with query ok", "https://hooks.example.com/pg?x=1", publicIP, false},
		{"no resolver ok", "https://hooks.example.com/pg", nil, false},
		{"http rejected", "http://hooks.example.com/pg", publicIP, true},
		{"ftp rejected", "ftp://hooks.example.com/pg", publicIP, true},
		{"empty", "", publicIP, true},
		{"relative", "/pg", publicIP, true},
		{"port 8080 rejected", "https://hooks.example.com:8080/pg", publicIP, true},
		{"ip literal rejected", "https://93.184.216.34/pg", publicIP, true},
		{"ipv6 literal rejected", "https://[2001:db8::1]/pg", publicIP, true},
		{"localhost rejected", "https://localhost/pg", publicIP, true},
		{"single label rejected", "https://intranet/pg", publicIP, true},
		{".internal rejected", "https://db.internal/pg", publicIP, true},
		{".local rejected", "https://printer.local/pg", publicIP, true},
		{"credentials rejected", "https://u:p@hooks.example.com/pg", publicIP, true},
		{"fragment rejected", "https://hooks.example.com/pg#frag", publicIP, true},
		{"resolves private", "https://hooks.example.com/pg", privateIP, true},
		{"resolves metadata", "https://hooks.example.com/pg", metadataIP, true},
		{"nxdomain", "https://hooks.example.com/pg", nxdomain, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := URLPolicy{Resolver: tt.resolver}
			err := p.Validate(context.Background(), tt.url)
			if tt.wantErr {
				require.Error(t, err)
				if tt.url != "" {
					assert.ErrorIs(t, err, ErrWebhookURLInvalid)
				}
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestURLPolicyDev(t *testing.T) {
	p := URLPolicy{AllowInsecure: true}
	for _, u := range []string{"http://localhost:8080/hook", "http://127.0.0.1:3000/hook", "https://hooks.example.com/pg", "http://host.docker.internal:9000/x"} {
		assert.NoError(t, p.Validate(context.Background(), u), u)
	}
	require.Error(t, p.Validate(context.Background(), "ftp://localhost/x"))
	require.Error(t, p.Validate(context.Background(), "not a url"))
}

func TestIsPrivateIP(t *testing.T) {
	private := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fc00::1", "fe80::1", "224.0.0.1"}
	for _, s := range private {
		assert.True(t, IsPrivateIP(net.ParseIP(s)), s)
	}
	public := []string{"8.8.8.8", "93.184.216.34", "2606:4700::1111", "100.128.0.1"}
	for _, s := range public {
		assert.False(t, IsPrivateIP(net.ParseIP(s)), s)
	}
	assert.True(t, IsPrivateIP(nil))
}

func TestValidateEnabledEvents(t *testing.T) {
	got, err := ValidateEnabledEvents(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, got)

	got, err = ValidateEnabledEvents([]string{"payment.captured", "payment.captured", "refund.*"})
	require.NoError(t, err)
	assert.Equal(t, []string{"payment.captured", "refund.*"}, got)

	got, err = ValidateEnabledEvents([]string{"payment.captured", "*"})
	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, got)

	_, err = ValidateEnabledEvents([]string{"payment.teleported"})
	require.ErrorIs(t, err, ErrParameterInvalid)
	_, err = ValidateEnabledEvents([]string{"nope.*"})
	require.Error(t, err)
}

func TestWebhookEndpointRotation(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	e, err := NewWebhookEndpoint(NewWebhookEndpointParams{
		MerchantID: uuid.New(), URL: "https://hooks.example.com/pg", Mode: ModeLive, SecretEnc: "enc-1",
	}, now)
	require.NoError(t, err)
	assert.Equal(t, EndpointEnabled, e.Status)
	assert.Equal(t, []string{"*"}, e.EnabledEvents)
	assert.Nil(t, e.PreviousSecretExpiresAt())
	assert.False(t, e.PreviousSecretValid(now))

	e.RotateSecret("enc-2", now)
	assert.Equal(t, "enc-2", e.SecretCurrentEnc)
	assert.Equal(t, "enc-1", e.SecretPreviousEnc)
	require.NotNil(t, e.PreviousSecretExpiresAt())
	assert.Equal(t, now.Add(SecretRotationGrace), *e.PreviousSecretExpiresAt())
	assert.True(t, e.PreviousSecretValid(now.Add(SecretRotationGrace-time.Second)))
	assert.False(t, e.PreviousSecretValid(now.Add(SecretRotationGrace)))

	assert.False(t, e.ExpirePreviousSecret(now.Add(time.Hour)))
	assert.True(t, e.ExpirePreviousSecret(now.Add(SecretRotationGrace+time.Second)))
	assert.Empty(t, e.SecretPreviousEnc)

	// 第二次輪替：previous 被新的 current 取代
	e.RotateSecret("enc-3", now.Add(48*time.Hour))
	assert.Equal(t, "enc-2", e.SecretPreviousEnc)
}

func TestWebhookEndpointLifecycle(t *testing.T) {
	now := time.Now()
	e, err := NewWebhookEndpoint(NewWebhookEndpointParams{MerchantID: uuid.New(), URL: "https://h.example.com/", Mode: ModeTest, SecretEnc: "x"}, now)
	require.NoError(t, err)
	require.NoError(t, e.Disable())
	assert.Equal(t, EndpointDisabled, e.Status)
	e.AutoDisable()
	assert.True(t, e.AutoDisabled)
	require.NoError(t, e.Enable())
	assert.False(t, e.AutoDisabled)
	assert.True(t, e.MarkDeleted(now))
	assert.False(t, e.MarkDeleted(now))
	assert.True(t, e.IsDeleted())
	require.ErrorIs(t, e.Enable(), ErrInvalidStateTransition)

	_, err = NewWebhookEndpoint(NewWebhookEndpointParams{MerchantID: uuid.New(), URL: "https://h.example.com/", Mode: "x", SecretEnc: "x"}, now)
	require.Error(t, err)
	_, err = NewWebhookEndpoint(NewWebhookEndpointParams{MerchantID: uuid.New(), URL: "https://h.example.com/", Mode: ModeTest}, now)
	require.Error(t, err, "缺 secret")
}

package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaskPAN(t *testing.T) {
	assert.Equal(t, "424242******4242", MaskPAN("4242424242424242"))
	assert.Equal(t, "424242******4242", MaskPAN("4242 4242 4242 4242"))
	assert.Equal(t, "*****", MaskPAN("12345"))
	assert.Empty(t, MaskPAN(""))
}

func TestMaskSecret(t *testing.T) {
	assert.Empty(t, MaskSecret(""))
	assert.Equal(t, "***", MaskSecret("abc"))
	assert.Equal(t, "pk_t******", MaskSecret("pk_test_ab"))
	assert.Equal(t, "pk_test_…St90", MaskSecret("pk_test_ab12Cd34Ef56Gh78Ij90Kl12Mn34Op56Qr78St90"))
}

func TestLoggerContext(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithWriter(&buf, "svc", "dev", "debug")
	ctx := IntoContext(WithRequestID(context.Background(), "req_1"), l)
	FromContext(ctx).Info("hello", "k", "v")

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "svc", rec["service"])
	assert.Equal(t, "dev", rec["env"])
	assert.Equal(t, "req_1", rec["request_id"])
	assert.Equal(t, "v", rec["k"])
	assert.Equal(t, "req_1", RequestIDFromContext(ctx))
	assert.Empty(t, TraceIDFromContext(ctx))
	assert.NotNil(t, FromContext(context.Background()))
}

func TestParseLevel(t *testing.T) {
	assert.Equal(t, slog.LevelDebug, ParseLevel("DEBUG"))
	assert.Equal(t, slog.LevelWarn, ParseLevel("warning"))
	assert.Equal(t, slog.LevelError, ParseLevel("error"))
	assert.Equal(t, slog.LevelInfo, ParseLevel("whatever"))
}

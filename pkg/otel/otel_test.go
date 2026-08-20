package otel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
)

func TestSetupNoop(t *testing.T) {
	ctx := context.Background()
	shutdown, err := Setup(ctx, "test-svc", "", Options{Version: "dev", Env: "dev"})
	require.NoError(t, err)
	_, span := otel.Tracer("t").Start(ctx, "op")
	span.End()
	require.NoError(t, shutdown(ctx))
}

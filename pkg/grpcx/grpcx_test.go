package grpcx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/pgdb"
)

func TestErrorFromDomain(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode codes.Code
		wantType string
		wantErrC string
	}{
		{"nil", nil, codes.OK, "", ""},
		{"not found", fmt.Errorf("x: %w", apperr.ErrResourceMissing), codes.NotFound, apperr.TypeInvalidRequest, "resource_missing"},
		{"invalid", apperr.New(apperr.TypeInvalidRequest, "amount_too_small", "too small").WithParam("amount"), codes.InvalidArgument, apperr.TypeInvalidRequest, "amount_too_small"},
		{"idempotency", apperr.ErrIdempotencyMismatch, codes.Aborted, apperr.TypeIdempotency, "idempotency_key_payload_mismatch"},
		{"state", apperr.New(apperr.TypeInvalidRequest, "invalid_state_transition", "bad"), codes.FailedPrecondition, apperr.TypeInvalidRequest, "invalid_state_transition"},
		{"provider declined", apperr.New(apperr.TypeProvider, "card_declined", "declined"), codes.FailedPrecondition, apperr.TypeProvider, "card_declined"},
		{"provider unavailable", apperr.New(apperr.TypeProvider, "provider_unavailable", "down"), codes.Unavailable, apperr.TypeProvider, "provider_unavailable"},
		{"rate", apperr.ErrRateLimited, codes.ResourceExhausted, apperr.TypeRateLimit, "rate_limit_exceeded"},
		{"auth", apperr.New(apperr.TypeAuthentication, "api_key_invalid", "x"), codes.Unauthenticated, apperr.TypeAuthentication, "api_key_invalid"},
		{"forbidden", apperr.New(apperr.TypeAuthentication, "merchant_suspended", "x"), codes.PermissionDenied, apperr.TypeAuthentication, "merchant_suspended"},
		{"concurrent", apperr.ErrConcurrentModify, codes.Aborted, apperr.TypeAPI, "concurrent_modification"},
		{"pgdb not found", pgdb.ErrNotFound, codes.NotFound, apperr.TypeInvalidRequest, "resource_missing"},
		{"pgdb concurrent", pgdb.ErrConcurrentModification, codes.Aborted, apperr.TypeAPI, "concurrent_modification"},
		{"plain", errors.New("boom"), codes.Internal, apperr.TypeAPI, "internal_error"},
		{"canceled", context.Canceled, codes.Canceled, "", ""},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded, "", ""},
		{"already status", status.Error(codes.Unimplemented, "nope"), codes.Unimplemented, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ErrorFromDomain(tt.err)
			if tt.err == nil {
				assert.NoError(t, got)
				return
			}
			st, ok := status.FromError(got)
			require.True(t, ok)
			assert.Equal(t, tt.wantCode, st.Code())
			ed := ErrorDetailFromStatus(st)
			if tt.wantType == "" {
				assert.Nil(t, ed)
				return
			}
			require.NotNil(t, ed)
			assert.Equal(t, tt.wantType, ed.GetType())
			assert.Equal(t, tt.wantErrC, ed.GetCode())
			// 來回轉換。
			back := ToAppError(got)
			assert.Equal(t, tt.wantErrC, back.Code)
		})
	}
	// 不洩漏內部訊息。
	st, _ := status.FromError(ErrorFromDomain(errors.New("secret db password")))
	assert.NotContains(t, st.Message(), "secret")
	assert.Equal(t, "amount", ErrorDetailFromStatus(status.Convert(ErrorFromDomain(apperr.ErrParameterInvalid.WithParam("amount")))).GetParam())
}

func TestToAppErrorWithoutDetail(t *testing.T) {
	assert.Equal(t, "resource_missing", ToAppError(status.Error(codes.NotFound, "x")).Code)
	assert.Equal(t, "service_unavailable", ToAppError(status.Error(codes.Unavailable, "x")).Code)
	assert.Equal(t, "timeout", ToAppError(status.Error(codes.DeadlineExceeded, "x")).Code)
	assert.Equal(t, "internal_error", ToAppError(status.Error(codes.Internal, "x")).Code)
	assert.Equal(t, "not_implemented", ToAppError(status.Error(codes.Unimplemented, "x")).Code)
	assert.Equal(t, "internal_error", ToAppError(errors.New("plain")).Code)
	assert.Nil(t, ToAppError(nil))
}

func TestServerHealthAndServe(t *testing.T) {
	srv, hs := NewServer(ServerOptions{EnableReflection: true})
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, lis, time.Second) }()

	conn, err := Dial(ctx, lis.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())

	cancel()
	require.NoError(t, <-done)
}

func TestRecoveryInterceptor(t *testing.T) {
	ic := RecoveryUnaryInterceptor(nil)
	_, err := ic(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x"}, func(context.Context, any) (any, error) { panic("boom") })
	assert.Equal(t, codes.Internal, status.Code(err))
}

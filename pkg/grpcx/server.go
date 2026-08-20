// Package grpcx 提供 gRPC server / client 建構、共用 interceptors 與領域錯誤 ↔ gRPC status 轉換。
package grpcx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/tenghongzou/paymentgateway/pkg/logx"
)

// MetadataRequestID 為跨服務傳遞 request id 的 metadata key。
const MetadataRequestID = "x-request-id"

// ServerOptions 為 NewServer 的選項。
type ServerOptions struct {
	Logger *slog.Logger
	// EnableReflection 在 dev / staging 開啟（docs/07 §1.8 第 2 點）。
	EnableReflection bool
	// ExtraUnary 追加在預設 interceptor 之後。
	ExtraUnary []grpc.UnaryServerInterceptor
}

// NewServer 建立帶 otelgrpc、recovery、request-id、logging interceptors 與 health / reflection 服務的 gRPC server。
func NewServer(opts ServerOptions) (*grpc.Server, *health.Server) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	unary := make([]grpc.UnaryServerInterceptor, 0, 3+len(opts.ExtraUnary))
	unary = append(unary,
		RecoveryUnaryInterceptor(opts.Logger),
		RequestIDUnaryInterceptor(opts.Logger),
		LoggingUnaryInterceptor(),
	)
	unary = append(unary, opts.ExtraUnary...)
	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(unary...),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	if opts.EnableReflection {
		reflection.Register(srv)
	}
	return srv, hs
}

// Serve 在 lis 上服務直到 ctx 結束，然後 GracefulStop（超過 drainTimeout 則強制 Stop）。
func Serve(ctx context.Context, srv *grpc.Server, lis net.Listener, drainTimeout time.Duration) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpcx: serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(drainTimeout):
		srv.Stop()
	}
	<-errCh
	return nil
}

// Dial 建立帶 otelgrpc、keepalive 與預設 10s deadline 的 client 連線（明文；生產 mTLS 由 service mesh 或後續加入）。
func Dial(_ context.Context, addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	base := make([]grpc.DialOption, 0, 5+len(opts))
	base = append(base,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
		grpc.WithChainUnaryInterceptor(DefaultTimeoutUnaryClientInterceptor(10*time.Second), RequestIDUnaryClientInterceptor()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
	)
	base = append(base, opts...)
	conn, err := grpc.NewClient(addr, base...)
	if err != nil {
		return nil, fmt.Errorf("grpcx: dial %s: %w", addr, err)
	}
	return conn, nil
}

// RecoveryUnaryInterceptor 把 panic 轉成 codes.Internal。
func RecoveryUnaryInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("grpc handler panic", "method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// RequestIDUnaryInterceptor 從 metadata 取 x-request-id 放入 context（並把 logger 放入 context）。
func RequestIDUnaryInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if v := md.Get(MetadataRequestID); len(v) > 0 {
				ctx = logx.WithRequestID(ctx, v[0])
			}
		}
		ctx = logx.IntoContext(ctx, log)
		return handler(ctx, req)
	}
}

// LoggingUnaryInterceptor 記錄每個 RPC 的方法、狀態碼與耗時。
func LoggingUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)
		log := logx.FromContext(ctx)
		attrs := []any{"grpc.method", info.FullMethod, "grpc.code", code.String(), "duration_ms", time.Since(start).Milliseconds()}
		switch {
		case err == nil:
			log.Debug("grpc request", attrs...)
		case code == codes.Internal || code == codes.Unknown || code == codes.DataLoss:
			log.Error("grpc request failed", append(attrs, "err", err)...)
		default:
			log.Info("grpc request rejected", append(attrs, "err", err)...)
		}
		return resp, err
	}
}

// DefaultTimeoutUnaryClientInterceptor 在呼叫端沒有 deadline 時補上預設逾時。
func DefaultTimeoutUnaryClientInterceptor(d time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// RequestIDUnaryClientInterceptor 把 context 中的 request id 放進 outgoing metadata。
func RequestIDUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if rid := logx.RequestIDFromContext(ctx); rid != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, MetadataRequestID, rid)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

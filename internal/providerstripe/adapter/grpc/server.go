// Package grpc 為 provider-stripe 的 gRPC adapter。
package grpc

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	providerv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/provider/v1"
)

// Server 實作 pg.provider.v1.ProviderAdapter（除 HealthCheck 外全部 Unimplemented；TODO: 由 provider-stripe 實作者補齊）。
type Server struct {
	providerv1.UnimplementedProviderAdapterServer
	version string
}

// NewServer 建立 Server。
func NewServer(version string) *Server { return &Server{version: version} }

// Register 把服務註冊到 gRPC server。
func (s *Server) Register(srv *grpc.Server) {
	providerv1.RegisterProviderAdapterServer(srv, s)
}

// HealthCheck 回報 NOT_SERVING，讓 payment-service 的路由不會把流量送到尚未實作的 adapter。
func (s *Server) HealthCheck(context.Context, *providerv1.HealthCheckRequest) (*providerv1.HealthCheckResponse, error) {
	return &providerv1.HealthCheckResponse{
		Status:         providerv1.HealthStatus_HEALTH_STATUS_NOT_SERVING,
		Provider:       "stripe",
		AdapterVersion: s.version,
		Message:        "provider-stripe is a skeleton; Stripe integration is not implemented yet",
		CheckedAt:      timestamppb.Now(),
	}, nil
}

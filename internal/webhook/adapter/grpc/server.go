// Package grpc 為 webhook-service 的 gRPC adapter：pg.webhook.v1.WebhookService 實作，
// 以及呼叫 merchant-service 取得端點 / 停用端點的 client（app.EndpointSource / app.EndpointDisabler）。
package grpc

import (
	"context"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	webhookv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/webhook/v1"
	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
)

// Server 實作 pg.webhook.v1.WebhookService。
type Server struct {
	webhookv1.UnimplementedWebhookServiceServer
	svc *app.Service
}

// NewServer 建立 Server。
func NewServer(svc *app.Service) *Server { return &Server{svc: svc} }

// Register 把服務註冊到 gRPC server。
func (s *Server) Register(srv *grpc.Server) {
	webhookv1.RegisterWebhookServiceServer(srv, s)
}

// ListDeliveries 依條件分頁列出（created_at DESC）。
func (s *Server) ListDeliveries(ctx context.Context, req *webhookv1.ListDeliveriesRequest) (*webhookv1.ListDeliveriesResponse, error) {
	merchantID, err := parseRequired(req.GetMerchantId(), "merchant_id", domain.ParseMerchantID)
	if err != nil {
		return nil, err
	}
	f := app.DeliveryFilter{
		MerchantID: merchantID,
		EventType:  req.GetEventType(),
		PageSize:   int(req.GetPage().GetPageSize()),
		PageToken:  req.GetPage().GetPageToken(),
	}
	if v := req.GetEndpointId(); v != "" {
		id, err := parseRequired(v, "endpoint_id", domain.ParseEndpointID)
		if err != nil {
			return nil, err
		}
		f.EndpointID = &id
	}
	if v := req.GetEventId(); v != "" {
		id, err := parseRequired(v, "event_id", domain.ParseEventID)
		if err != nil {
			return nil, err
		}
		f.EventID = &id
	}
	for _, st := range req.GetStatuses() {
		ds, ok := statusFromProto(st)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "statuses: unknown value %s", st)
		}
		f.Statuses = append(f.Statuses, ds)
	}
	if req.GetCreatedAfter().IsValid() {
		t := req.GetCreatedAfter().AsTime()
		f.CreatedAfter = &t
	}
	if req.GetCreatedBefore().IsValid() {
		t := req.GetCreatedBefore().AsTime()
		f.CreatedBefore = &t
	}
	if req.Livemode != nil {
		lm := req.GetLivemode()
		f.Livemode = &lm
	}
	page, err := s.svc.ListDeliveries(ctx, f)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	out := &webhookv1.ListDeliveriesResponse{
		Deliveries: make([]*webhookv1.Delivery, 0, len(page.Deliveries)),
		Page:       &commonv1.PageResponse{NextPageToken: page.NextPageToken, HasMore: page.NextPageToken != ""},
	}
	urls := map[uuid.UUID]string{}
	for _, d := range page.Deliveries {
		url, ok := urls[d.EndpointID]
		if !ok {
			if ep := s.svc.LookupEndpoint(ctx, d.MerchantID, d.EndpointID); ep != nil {
				url = ep.URL
			}
			urls[d.EndpointID] = url
		}
		out.Deliveries = append(out.Deliveries, toProto(d, url, nil, false))
	}
	return out, nil
}

// GetDelivery 取得 delivery（含 payload 與 attempts）。
func (s *Server) GetDelivery(ctx context.Context, req *webhookv1.GetDeliveryRequest) (*webhookv1.GetDeliveryResponse, error) {
	merchantID, deliveryID, err := parseMerchantAndDelivery(req.GetMerchantId(), req.GetDeliveryId())
	if err != nil {
		return nil, err
	}
	d, atts, err := s.svc.GetDelivery(ctx, merchantID, deliveryID)
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &webhookv1.GetDeliveryResponse{Delivery: toProto(d, s.endpointURL(ctx, d), atts, true)}, nil
}

// RetryDelivery 手動重送。
func (s *Server) RetryDelivery(ctx context.Context, req *webhookv1.RetryDeliveryRequest) (*webhookv1.RetryDeliveryResponse, error) {
	merchantID, deliveryID, err := parseMerchantAndDelivery(req.GetMerchantId(), req.GetDeliveryId())
	if err != nil {
		return nil, err
	}
	if req.GetIdempotencyKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency_key is required")
	}
	d, err := s.svc.RetryDelivery(ctx, merchantID, deliveryID, req.GetIdempotencyKey())
	if err != nil {
		return nil, grpcx.ErrorFromDomain(err)
	}
	return &webhookv1.RetryDeliveryResponse{Delivery: toProto(d, s.endpointURL(ctx, d), nil, false)}, nil
}

// ListEventTypes 列出支援的事件類型。
func (s *Server) ListEventTypes(context.Context, *webhookv1.ListEventTypesRequest) (*webhookv1.ListEventTypesResponse, error) {
	types := s.svc.ListEventTypes()
	out := &webhookv1.ListEventTypesResponse{EventTypes: make([]*webhookv1.EventTypeInfo, 0, len(types))}
	for _, t := range types {
		out.EventTypes = append(out.EventTypes, &webhookv1.EventTypeInfo{
			Name: t.Name, Description: t.Description, ObjectType: t.ObjectType, Terminal: t.Terminal,
		})
	}
	return out, nil
}

func (s *Server) endpointURL(ctx context.Context, d *domain.Delivery) string {
	if ep := s.svc.LookupEndpoint(ctx, d.MerchantID, d.EndpointID); ep != nil {
		return ep.URL
	}
	return ""
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseRequired(v, field string, parse func(string) (uuid.UUID, error)) (uuid.UUID, error) {
	if v == "" {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	id, err := parse(v)
	if err != nil {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "%s is invalid", field)
	}
	return id, nil
}

func parseMerchantAndDelivery(merchant, delivery string) (uuid.UUID, uuid.UUID, error) {
	merchantID, err := parseRequired(merchant, "merchant_id", domain.ParseMerchantID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if delivery == "" {
		return uuid.Nil, uuid.Nil, status.Error(codes.InvalidArgument, "delivery_id is required")
	}
	deliveryID, err := domain.ParseDeliveryID(delivery)
	if err != nil {
		// 格式不對的 ID 視同不存在（不洩漏格式資訊給跨商戶探測）。
		return uuid.Nil, uuid.Nil, status.Error(codes.NotFound, domain.ErrDeliveryNotFound.Message)
	}
	return merchantID, deliveryID, nil
}

func statusFromProto(s webhookv1.DeliveryStatus) (domain.DeliveryStatus, bool) {
	switch s {
	case webhookv1.DeliveryStatus_DELIVERY_STATUS_PENDING:
		return domain.StatusPending, true
	case webhookv1.DeliveryStatus_DELIVERY_STATUS_IN_FLIGHT:
		return domain.StatusInFlight, true
	case webhookv1.DeliveryStatus_DELIVERY_STATUS_SUCCEEDED:
		return domain.StatusSucceeded, true
	case webhookv1.DeliveryStatus_DELIVERY_STATUS_FAILED:
		return domain.StatusFailed, true
	case webhookv1.DeliveryStatus_DELIVERY_STATUS_DEAD_LETTER:
		return domain.StatusDeadLetter, true
	case webhookv1.DeliveryStatus_DELIVERY_STATUS_CANCELED:
		return domain.StatusCanceled, true
	default:
		return "", false
	}
}

func statusToProto(s domain.DeliveryStatus) webhookv1.DeliveryStatus {
	switch s {
	case domain.StatusPending:
		return webhookv1.DeliveryStatus_DELIVERY_STATUS_PENDING
	case domain.StatusInFlight:
		return webhookv1.DeliveryStatus_DELIVERY_STATUS_IN_FLIGHT
	case domain.StatusSucceeded:
		return webhookv1.DeliveryStatus_DELIVERY_STATUS_SUCCEEDED
	case domain.StatusFailed:
		return webhookv1.DeliveryStatus_DELIVERY_STATUS_FAILED
	case domain.StatusDeadLetter:
		return webhookv1.DeliveryStatus_DELIVERY_STATUS_DEAD_LETTER
	case domain.StatusCanceled:
		return webhookv1.DeliveryStatus_DELIVERY_STATUS_CANCELED
	default:
		return webhookv1.DeliveryStatus_DELIVERY_STATUS_UNSPECIFIED
	}
}

func tsPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(*t)
}

// toProto 把 domain.Delivery 轉成 pg.webhook.v1.Delivery。
func toProto(d *domain.Delivery, endpointURL string, atts []*domain.Attempt, includePayload bool) *webhookv1.Delivery {
	out := &webhookv1.Delivery{
		Id:              d.PublicID(),
		MerchantId:      domain.MerchantPublicID(d.MerchantID),
		EndpointId:      domain.EndpointPublicID(d.EndpointID),
		EndpointUrl:     endpointURL,
		EventId:         domain.EventPublicID(d.EventID),
		EventType:       d.EventType,
		Status:          statusToProto(d.Status),
		AttemptCount:    int32(d.AttemptNo),
		MaxAttempts:     domain.MaxAttempts,
		LastAttemptedAt: tsPtr(d.LastAttemptAt),
		Livemode:        d.Livemode,
		CreatedAt:       timestamppb.New(d.CreatedAt),
		UpdatedAt:       timestamppb.New(d.UpdatedAt),
		DeliveredAt:     tsPtr(d.DeliveredAt),
	}
	if d.Status == domain.StatusPending || d.Status == domain.StatusFailed {
		out.NextRetryAt = timestamppb.New(d.NextAttemptAt)
	}
	if d.Status == domain.StatusDeadLetter {
		out.DeadLetteredAt = timestamppb.New(d.UpdatedAt)
	}
	if d.LastResponseStatus != nil {
		out.LastResponseStatus = int32(*d.LastResponseStatus)
	}
	if d.LastError != nil {
		out.LastError = *d.LastError
	}
	if includePayload {
		out.Payload = d.EventPayload
	}
	for _, a := range atts {
		pa := &webhookv1.DeliveryAttempt{
			AttemptNumber: int32(a.AttemptNo),
			AttemptedAt:   timestamppb.New(a.AttemptedAt),
			DurationMs:    int64(a.DurationMS),
			Succeeded:     a.Succeeded(),
		}
		if a.ResponseStatus != nil {
			pa.ResponseStatus = int32(*a.ResponseStatus)
		}
		if a.ResponseBody != nil {
			// proto 註解：回應 body 前 1KB。
			pa.ResponseBodySnippet = snippet(*a.ResponseBody, 1024)
		}
		if a.Error != nil {
			pa.Error = *a.Error
		}
		out.Attempts = append(out.Attempts, pa)
	}
	return out
}

func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

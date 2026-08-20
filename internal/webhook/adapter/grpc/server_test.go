package grpc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/common/v1"
	merchantv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/merchant/v1"
	webhookv1 "github.com/tenghongzou/paymentgateway/api/gen/go/pg/webhook/v1"
	"github.com/tenghongzou/paymentgateway/internal/webhook/app"
	"github.com/tenghongzou/paymentgateway/internal/webhook/app/apptest"
	"github.com/tenghongzou/paymentgateway/internal/webhook/domain"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
)

type env struct {
	store  *apptest.MemStore
	eps    *apptest.MemEndpoints
	sender *apptest.ScriptedSender
	clock  *apptest.FakeClock
	svc    *app.Service
	client webhookv1.WebhookServiceClient
	merch  uuid.UUID
	ep     *domain.Endpoint
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		store: apptest.NewMemStore(), eps: apptest.NewMemEndpoints(), sender: &apptest.ScriptedSender{},
		clock: apptest.NewFakeClock(time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)), merch: uuid.New(),
	}
	e.ep = &domain.Endpoint{ID: uuid.New(), MerchantID: e.merch, URL: "https://m.example.com/h", Secrets: []string{"s"}, Status: domain.EndpointEnabled, Livemode: true}
	e.eps.Add(e.ep)
	e.svc = app.New(app.Deps{
		Tx: e.store, Inbox: e.store, Events: &apptest.MemEventRepo{MemStore: e.store}, Deliveries: e.store, Endpoints: e.eps,
		Disabler: e.eps, Sender: e.sender, Clock: e.clock, Policy: domain.StrictPolicy, Rand: func() float64 { return 0.5 },
	})

	lis := bufconn.Listen(1 << 20)
	srv, _ := grpcx.NewServer(grpcx.ServerOptions{})
	NewServer(e.svc).Register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	e.client = webhookv1.NewWebhookServiceClient(conn)
	return e
}

func (e *env) ingest(t *testing.T, typ string) *domain.Event {
	t.Helper()
	id := domain.NewEventID()
	ev := &domain.Event{ID: id, MerchantID: e.merch, Type: typ, ResourceType: "payment", ResourceID: "pay_1", PaymentID: "pay_1",
		Livemode: true, Payload: []byte(`{"id":"` + domain.EventPublicID(id) + `","livemode":true}`), OccurredAt: e.clock.Now(), CreatedAt: e.clock.Now()}
	_, err := e.svc.IngestEvent(context.Background(), ev)
	require.NoError(t, err)
	e.clock.Advance(time.Millisecond)
	return ev
}

func TestServer_ListGetRetry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ev := e.ingest(t, "payment.captured")
	e.ingest(t, "payment.failed")
	e.sender.Outcomes = []domain.Outcome{{StatusCode: 500, Body: "boom"}, {StatusCode: 200, Body: "ok"}}
	// concurrency=1 讓取件順序（next_attempt_at）決定誰拿到 500：第一個事件。
	_, err := e.svc.DispatchDue(ctx, 10, 1)
	require.NoError(t, err)

	// List：全部。
	resp, err := e.client.ListDeliveries(ctx, &webhookv1.ListDeliveriesRequest{MerchantId: domain.MerchantPublicID(e.merch)})
	require.NoError(t, err)
	assert.Len(t, resp.GetDeliveries(), 2)
	for _, d := range resp.GetDeliveries() {
		assert.Equal(t, "https://m.example.com/h", d.GetEndpointUrl())
		assert.Equal(t, domain.EndpointPublicID(e.ep.ID), d.GetEndpointId())
		assert.EqualValues(t, 10, d.GetMaxAttempts())
		assert.Empty(t, d.GetPayload(), "List 不帶 payload")
		assert.True(t, d.GetLivemode())
	}
	// 依狀態篩選。
	resp, err = e.client.ListDeliveries(ctx, &webhookv1.ListDeliveriesRequest{
		MerchantId: domain.MerchantPublicID(e.merch), Statuses: []webhookv1.DeliveryStatus{webhookv1.DeliveryStatus_DELIVERY_STATUS_FAILED},
	})
	require.NoError(t, err)
	require.Len(t, resp.GetDeliveries(), 1)
	failed := resp.GetDeliveries()[0]
	assert.Equal(t, webhookv1.DeliveryStatus_DELIVERY_STATUS_FAILED, failed.GetStatus())
	assert.EqualValues(t, 1, failed.GetAttemptCount())
	assert.EqualValues(t, 500, failed.GetLastResponseStatus())
	assert.NotNil(t, failed.GetNextRetryAt())
	assert.Equal(t, domain.EventPublicID(ev.ID), failed.GetEventId())

	// Get：含 payload 與 attempts。
	got, err := e.client.GetDelivery(ctx, &webhookv1.GetDeliveryRequest{MerchantId: domain.MerchantPublicID(e.merch), DeliveryId: failed.GetId()})
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"`+domain.EventPublicID(ev.ID)+`","livemode":true}`, string(got.GetDelivery().GetPayload()))
	require.Len(t, got.GetDelivery().GetAttempts(), 1)
	assert.EqualValues(t, 500, got.GetDelivery().GetAttempts()[0].GetResponseStatus())
	assert.Equal(t, "boom", got.GetDelivery().GetAttempts()[0].GetResponseBodySnippet())
	assert.False(t, got.GetDelivery().GetAttempts()[0].GetSucceeded())

	// 跨商戶 → NOT_FOUND；格式錯誤 ID → NOT_FOUND；缺 merchant → INVALID_ARGUMENT。
	_, err = e.client.GetDelivery(ctx, &webhookv1.GetDeliveryRequest{MerchantId: domain.MerchantPublicID(uuid.New()), DeliveryId: failed.GetId()})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = e.client.GetDelivery(ctx, &webhookv1.GetDeliveryRequest{MerchantId: domain.MerchantPublicID(e.merch), DeliveryId: "whd_nope"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = e.client.GetDelivery(ctx, &webhookv1.GetDeliveryRequest{DeliveryId: failed.GetId()})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = e.client.ListDeliveries(ctx, &webhookv1.ListDeliveriesRequest{MerchantId: "bogus"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// Retry：failed 可重送 → PENDING；缺 idempotency_key → INVALID_ARGUMENT。
	_, err = e.client.RetryDelivery(ctx, &webhookv1.RetryDeliveryRequest{MerchantId: domain.MerchantPublicID(e.merch), DeliveryId: failed.GetId()})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	rr, err := e.client.RetryDelivery(ctx, &webhookv1.RetryDeliveryRequest{MerchantId: domain.MerchantPublicID(e.merch), DeliveryId: failed.GetId(), IdempotencyKey: "k1"})
	require.NoError(t, err)
	assert.Equal(t, webhookv1.DeliveryStatus_DELIVERY_STATUS_PENDING, rr.GetDelivery().GetStatus())
	assert.EqualValues(t, 0, rr.GetDelivery().GetAttemptCount())
	// pending 再重送 → FAILED_PRECONDITION。
	_, err = e.client.RetryDelivery(ctx, &webhookv1.RetryDeliveryRequest{MerchantId: domain.MerchantPublicID(e.merch), DeliveryId: failed.GetId(), IdempotencyKey: "k2"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	// 端點停用 → FAILED_PRECONDITION（先讓它成功再停用）。
	_, err = e.svc.DispatchDue(ctx, 10, 2)
	require.NoError(t, err)
	e.ep.Status = domain.EndpointDisabled
	_, err = e.client.RetryDelivery(ctx, &webhookv1.RetryDeliveryRequest{MerchantId: domain.MerchantPublicID(e.merch), DeliveryId: failed.GetId(), IdempotencyKey: "k3"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// ListEventTypes。
	et, err := e.client.ListEventTypes(ctx, &webhookv1.ListEventTypesRequest{})
	require.NoError(t, err)
	assert.Len(t, et.GetEventTypes(), 14)
	assert.Equal(t, "payment.captured", et.GetEventTypes()[3].GetName())
	assert.Equal(t, "payment", et.GetEventTypes()[3].GetObjectType())
}

func TestToProto_DeadLetter(t *testing.T) {
	now := time.Now().UTC()
	code := 503
	d := &domain.Delivery{ID: uuid.New(), EventID: uuid.New(), EndpointID: uuid.New(), MerchantID: uuid.New(),
		AttemptNo: 10, Status: domain.StatusDeadLetter, NextAttemptAt: now, LastAttemptAt: &now, LastResponseStatus: &code,
		CreatedAt: now, UpdatedAt: now, EventType: "payment.captured", EventPayload: []byte(`{}`), Livemode: true}
	p := toProto(d, "", nil, true)
	assert.Equal(t, webhookv1.DeliveryStatus_DELIVERY_STATUS_DEAD_LETTER, p.GetStatus())
	assert.NotNil(t, p.GetDeadLetteredAt())
	assert.Nil(t, p.GetNextRetryAt())
	assert.EqualValues(t, 503, p.GetLastResponseStatus())
	assert.Equal(t, []byte(`{}`), p.GetPayload())
	assert.Equal(t, d.PublicID(), p.GetId())
}

// fakeMerchant 為 merchant-service 的最小假實作。
type fakeMerchant struct {
	merchantv1.UnimplementedMerchantServiceServer
	endpoints []*merchantv1.WebhookEndpoint
	updates   []*merchantv1.UpdateWebhookEndpointRequest
}

func (f *fakeMerchant) ListWebhookEndpoints(_ context.Context, req *merchantv1.ListWebhookEndpointsRequest) (*merchantv1.ListWebhookEndpointsResponse, error) {
	if !req.GetIncludeSecrets() {
		return nil, status.Error(codes.PermissionDenied, "secrets required")
	}
	// 模擬兩頁。
	if req.GetPage().GetPageToken() == "" {
		return &merchantv1.ListWebhookEndpointsResponse{Endpoints: f.endpoints[:1], Page: &commonv1.PageResponse{NextPageToken: "p2", HasMore: true}}, nil
	}
	return &merchantv1.ListWebhookEndpointsResponse{Endpoints: f.endpoints[1:], Page: &commonv1.PageResponse{}}, nil
}

func (f *fakeMerchant) UpdateWebhookEndpoint(_ context.Context, req *merchantv1.UpdateWebhookEndpointRequest) (*merchantv1.UpdateWebhookEndpointResponse, error) {
	f.updates = append(f.updates, req)
	return &merchantv1.UpdateWebhookEndpointResponse{Endpoint: req.GetEndpoint()}, nil
}

func TestMerchantEndpointSource(t *testing.T) {
	now := time.Now()
	ep1, ep2, ep3 := uuid.New(), uuid.New(), uuid.New()
	fm := &fakeMerchant{endpoints: []*merchantv1.WebhookEndpoint{
		{Id: domain.EndpointPublicID(ep1), Url: "https://a.example.com/h", EnabledEvents: []string{"payment.captured"},
			Status: merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_ENABLED, Mode: merchantv1.ApiKeyMode_API_KEY_MODE_LIVE,
			SigningSecret: "whsec_new", PreviousSigningSecret: "whsec_old", PreviousSecretExpiresAt: timestamppb.New(now.Add(time.Hour))},
		{Id: domain.EndpointPublicID(ep2), Url: "https://b.example.com/h",
			Status: merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_AUTO_DISABLED, Mode: merchantv1.ApiKeyMode_API_KEY_MODE_TEST,
			SigningSecret: "whsec_b", PreviousSigningSecret: "whsec_expired", PreviousSecretExpiresAt: timestamppb.New(now.Add(-time.Hour))},
		{Id: "bad-id", Url: "https://c.example.com/h", SigningSecret: "x"},
		{Id: domain.EndpointPublicID(ep3), Url: "https://c.example.com/h", Status: merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_ENABLED, SigningSecret: "whsec_c"},
	}}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	merchantv1.RegisterMerchantServiceServer(srv, fm)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	src := NewMerchantEndpointSource(conn, app.ClockFunc(func() time.Time { return now }))
	m := uuid.New()
	eps, err := src.ListEndpoints(context.Background(), m)
	require.NoError(t, err)
	require.Len(t, eps, 3, "兩頁合併、壞 ID 略過")
	assert.Equal(t, ep1, eps[0].ID)
	assert.Equal(t, m, eps[0].MerchantID)
	assert.Equal(t, []string{"whsec_new", "whsec_old"}, eps[0].Secrets, "輪替期間兩把")
	assert.Equal(t, domain.EndpointEnabled, eps[0].Status)
	assert.True(t, eps[0].Livemode)
	assert.Equal(t, []string{"payment.captured"}, eps[0].EnabledEvents)
	assert.Equal(t, []string{"whsec_b"}, eps[1].Secrets, "過期的 previous 不帶")
	assert.Equal(t, domain.EndpointAutoDisabled, eps[1].Status)
	assert.False(t, eps[1].Livemode)

	got, err := src.GetEndpoint(context.Background(), m, ep3)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "https://c.example.com/h", got.URL)
	none, err := src.GetEndpoint(context.Background(), m, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, none)

	require.NoError(t, src.DisableEndpoint(context.Background(), m, ep1, "410"))
	require.Len(t, fm.updates, 1)
	assert.Equal(t, domain.MerchantPublicID(m), fm.updates[0].GetMerchantId())
	assert.Equal(t, domain.EndpointPublicID(ep1), fm.updates[0].GetEndpoint().GetId())
	assert.Equal(t, merchantv1.WebhookEndpointStatus_WEBHOOK_ENDPOINT_STATUS_DISABLED, fm.updates[0].GetEndpoint().GetStatus())
	assert.Equal(t, []string{"status"}, fm.updates[0].GetUpdateMask().GetPaths())
}

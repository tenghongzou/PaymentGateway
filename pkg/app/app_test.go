package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/tenghongzou/paymentgateway/pkg/config"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
)

type testConfig struct {
	config.Base
	Extra string `env:"PG_TEST_EXTRA" envDefault:"x"`
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().String()
}

func TestMainServesAndShutsDown(t *testing.T) {
	httpAddr, grpcAddr := freePort(t), freePort(t)
	t.Setenv("PG_SERVICE_NAME", "test-svc")
	t.Setenv("PG_HTTP_ADDR", httpAddr)
	t.Setenv("PG_GRPC_ADDR", grpcAddr)
	t.Setenv("PG_SHUTDOWN_TIMEOUT", "2s")

	workerStopped := make(chan struct{})
	closed := make(chan struct{})
	var setupCfg testConfig
	setup := func(_ context.Context, rt *Runtime, cfg testConfig) (*Hooks, error) { //nolint:unparam // 簽章由 SetupFunc 決定
		setupCfg = cfg
		require.NotNil(t, rt.GRPC)
		return &Hooks{
			Ready: []Check{{Name: "always", Fn: func(context.Context) error { return nil }}},
			Workers: []Worker{{Name: "w", Run: func(ctx context.Context) error {
				<-ctx.Done()
				close(workerStopped)
				return nil
			}}},
			Closers: []Closer{{Name: "c", Close: func(context.Context) error { close(closed); return nil }}},
		}, nil
	}

	exit := make(chan int, 1)
	go func() { exit <- Main(Options{Info: Info{Version: "v1"}}, setup, nil) }()

	// 等待 /healthz 可用。
	var resp *http.Response
	require.Eventually(t, func() bool {
		r, err := http.Get("http://" + httpAddr + "/healthz")
		if err != nil {
			return false
		}
		resp = r
		return true
	}, 5*time.Second, 20*time.Millisecond)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "x", setupCfg.Extra)

	require.Eventually(t, func() bool {
		r, err := http.Get("http://" + httpAddr + "/readyz")
		if err != nil {
			return false
		}
		defer r.Body.Close()
		return r.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)

	mr, err := http.Get("http://" + httpAddr + "/metrics")
	require.NoError(t, err)
	_ = mr.Body.Close()
	assert.Equal(t, http.StatusOK, mr.StatusCode)

	conn, err := grpcx.Dial(context.Background(), grpcAddr)
	require.NoError(t, err)
	defer conn.Close()
	hc, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, hc.GetStatus())

	// 送 SIGTERM 給自己觸發優雅關機。
	p, err := findSelf()
	require.NoError(t, err)
	require.NoError(t, p.Signal(sigterm()))

	select {
	case code := <-exit:
		assert.Equal(t, 0, code)
	case <-time.After(10 * time.Second):
		t.Fatal("service did not shut down")
	}
	select {
	case <-workerStopped:
	default:
		t.Fatal("worker was not stopped")
	}
	select {
	case <-closed:
	default:
		t.Fatal("closer was not called")
	}
}

func TestMainSetupError(t *testing.T) {
	t.Setenv("PG_HTTP_ADDR", freePort(t))
	code := Main(Options{}, func(context.Context, *Runtime, testConfig) (*Hooks, error) {
		return nil, errors.New("nope")
	}, nil)
	assert.Equal(t, 1, code)
}

func TestMainMigrateWithoutDB(t *testing.T) {
	assert.Equal(t, 2, Main(Options{}, func(context.Context, *Runtime, testConfig) (*Hooks, error) { return nil, nil }, []string{"migrate", "up"}))
	t.Setenv("PG_DATABASE_URL", "")
	assert.Equal(t, 2, Main(Options{MigrationService: "payment"}, func(context.Context, *Runtime, testConfig) (*Hooks, error) { return nil, nil }, []string{"migrate", "up"}))
	assert.Equal(t, 2, Main(Options{MigrationService: "payment"}, func(context.Context, *Runtime, testConfig) (*Hooks, error) { return nil, nil }, []string{"migrate"}))
	assert.Equal(t, 2, Main(Options{MigrationService: "payment"}, func(context.Context, *Runtime, testConfig) (*Hooks, error) { return nil, nil }, []string{"migrate", "sideways"}))
	t.Setenv("PG_DATABASE_URL", "postgres://nobody:nobody@127.0.0.1:1/nodb?sslmode=disable&connect_timeout=1")
	assert.Equal(t, 1, Main(Options{MigrationService: "payment"}, func(context.Context, *Runtime, testConfig) (*Hooks, error) { return nil, nil }, []string{"migrate", "version"}))
}

func TestMainBadConfig(t *testing.T) {
	t.Setenv("PG_ENV", "nope")
	assert.Equal(t, 2, Main(Options{}, func(context.Context, *Runtime, testConfig) (*Hooks, error) { return nil, nil }, nil))
}

func TestWorkerFailureTriggersShutdown(t *testing.T) {
	t.Setenv("PG_HTTP_ADDR", freePort(t))
	t.Setenv("PG_GRPC_ADDR", "")
	code := Main(Options{}, func(context.Context, *Runtime, testConfig) (*Hooks, error) {
		return &Hooks{Workers: []Worker{{Name: "boom", Run: func(context.Context) error { return fmt.Errorf("worker failed") }}}}, nil
	}, nil)
	assert.Equal(t, 1, code)
}

package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tenghongzou/paymentgateway/pkg/apperr"
	"github.com/tenghongzou/paymentgateway/pkg/grpcx"
	"github.com/tenghongzou/paymentgateway/pkg/logx"
)

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusUnprocessableEntity, apperr.TypeInvalidRequest, "amount_too_small", "too small", "amount", "req_1")
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	var body ErrorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "invalid_request_error", body.Error.Type)
	assert.Equal(t, "amount_too_small", body.Error.Code)
	require.NotNil(t, body.Error.Param)
	assert.Equal(t, "amount", *body.Error.Param)
	assert.Equal(t, "req_1", body.Error.RequestID)

	rec = httptest.NewRecorder()
	WriteError(rec, http.StatusTooManyRequests, apperr.TypeRateLimit, "rate_limit_exceeded", "slow down", "", "req_2")
	assert.Equal(t, "1", rec.Header().Get(HeaderRetryAfter))
	assert.Contains(t, rec.Body.String(), `"param":null`)
}

func TestWriteAppErrorFromGRPC(t *testing.T) {
	grpcErr := grpcx.ErrorFromDomain(apperr.New(apperr.TypeInvalidRequest, "resource_missing", "no such payment").WithParam("id"))
	req := httptest.NewRequest(http.MethodGet, "/v1/payments/x", http.NoBody)
	req = req.WithContext(logx.WithRequestID(req.Context(), "req_9"))
	rec := httptest.NewRecorder()
	WriteAppError(rec, req, grpcErr)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"resource_missing"`)
	assert.Contains(t, rec.Body.String(), `"request_id":"req_9"`)

	rec = httptest.NewRecorder()
	WriteAppError(rec, req, status.Error(codes.Unavailable, "down"))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get(HeaderRetryAfter))

	rec = httptest.NewRecorder()
	WriteAppError(rec, req, errors.New("boom"))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "boom")
	assert.Equal(t, "internal_error", ErrorFromGRPC(nil).Code)
}

func TestMiddlewareChain(t *testing.T) {
	log := logx.NewWithWriter(io.Discard, "t", "dev", "debug")
	h := RequestID(Recover(log)(Logging(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("boom")
		}
		WriteJSON(w, http.StatusCreated, map[string]string{"rid": RequestIDFromRequest(r)})
	}))))

	req := httptest.NewRequest(http.MethodGet, "/ok", http.NoBody)
	req.Header.Set(HeaderRequestID, "req_custom")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "req_custom", rec.Header().Get(HeaderRequestID))
	assert.Contains(t, rec.Body.String(), "req_custom")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", http.NoBody))
	assert.True(t, strings.HasPrefix(rec.Header().Get(HeaderRequestID), "req_"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", http.NoBody))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "internal_error")
}

func TestTimeout(t *testing.T) {
	h := Timeout(20 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "timeout")
}

func TestDecodeJSONAndReadBody(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))
	body, err := ReadBody(req, 0)
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":1}`, string(body))
	require.NoError(t, DecodeJSON(req, &v))
	assert.Equal(t, 1, v.A)

	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{bad`))
	err = DecodeJSON(req, &v)
	require.ErrorIs(t, err, apperr.ErrParameterInvalid)

	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	require.NoError(t, DecodeJSON(req, &v))

	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789"))
	_, err = ReadBody(req, 5)
	require.Error(t, err)

	req = httptest.NewRequest(http.MethodGet, "/?limit=5&bad=x", http.NoBody)
	n, err := ParseIntQuery(req, "limit", 20)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	n, err = ParseIntQuery(req, "missing", 20)
	require.NoError(t, err)
	assert.Equal(t, 20, n)
	_, err = ParseIntQuery(req, "bad", 20)
	require.ErrorIs(t, err, apperr.ErrParameterInvalid)
}

package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPStatus(t *testing.T) {
	tests := []struct {
		typ, code string
		want      int
	}{
		{TypeInvalidRequest, "resource_missing", http.StatusNotFound},
		{TypeInvalidRequest, "amount_too_small", http.StatusUnprocessableEntity},
		{TypeInvalidRequest, "unknown_code", http.StatusBadRequest},
		{TypeAuthentication, "whatever", http.StatusUnauthorized},
		{TypeIdempotency, "idempotency_key_in_use", http.StatusConflict},
		{TypeRateLimit, "rate_limit_exceeded", http.StatusTooManyRequests},
		{TypeProvider, "provider_unavailable", http.StatusServiceUnavailable},
		{TypeProvider, "card_declined", http.StatusPaymentRequired},
		{TypeAPI, "concurrent_modification", http.StatusConflict},
		{TypeAPI, "x", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.typ+"/"+tt.code, func(t *testing.T) {
			assert.Equal(t, tt.want, HTTPStatus(tt.typ, tt.code))
		})
	}
}

func TestErrorIsAndWrap(t *testing.T) {
	base := ErrResourceMissing
	wrapped := fmt.Errorf("use case: %w", base.WithMessage("payment %s not found", "pay_1").WithParam("id"))
	require.ErrorIs(t, wrapped, ErrResourceMissing)
	e := From(wrapped)
	require.NotNil(t, e)
	assert.Equal(t, "id", e.Param)
	assert.Equal(t, http.StatusNotFound, e.HTTPStatus())
	assert.Nil(t, From(errors.New("plain")))

	cause := errors.New("boom")
	require.ErrorIs(t, ErrInternal.Wrap(cause), cause)
	assert.Contains(t, ErrInternal.Wrap(cause).Error(), "boom")
}

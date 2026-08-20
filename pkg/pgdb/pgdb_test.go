package pgdb

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestErrorHelpers(t *testing.T) {
	uniq := fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "23505", ConstraintName: "payments_merchant_idem_key"})
	check := &pgconn.PgError{Code: "23514"}
	fk := &pgconn.PgError{Code: "23503"}

	assert.True(t, IsUniqueViolation(uniq))
	assert.False(t, IsUniqueViolation(check))
	assert.True(t, IsCheckViolation(check))
	assert.True(t, IsForeignKeyViolation(fk))
	assert.False(t, IsUniqueViolation(errors.New("x")))
	assert.Equal(t, "payments_merchant_idem_key", ConstraintName(uniq))
	assert.Empty(t, ConstraintName(errors.New("x")))
	assert.True(t, IsNoRows(pgx.ErrNoRows))
	assert.True(t, IsNoRows(fmt.Errorf("w: %w", ErrNotFound)))
	assert.False(t, IsNoRows(errors.New("x")))
}

func TestConnectBadURL(t *testing.T) {
	_, err := Connect(t.Context(), "://bad")
	assert.Error(t, err)
}

package ids

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFormatParse(t *testing.T) {
	id := New(PrefixPayment)
	assert.Len(t, id, len(PrefixPayment)+1+26)
	assert.Regexp(t, `^pay_[0-9A-HJKMNP-TV-Z]{26}$`, id)

	prefix, u, err := Parse(id)
	require.NoError(t, err)
	assert.Equal(t, PrefixPayment, prefix)
	assert.Equal(t, id, Format(PrefixPayment, u))
	assert.Equal(t, uuid.Version(7), u.Version())

	// 時間可排序：後產生的 ID 字典序較大（UUIDv7 毫秒時戳 + 單調序列）。
	a, b := New("x"), New("x")
	assert.Less(t, a, b)
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"", "pay", "pay_", "_abc", "pay_tooshort", "pay_" + "I" + "0123456789ABCDEFGHJKMNPQR", "pay_01234567890123456789012345_x"} {
		_, _, err := Parse(bad)
		require.ErrorIs(t, err, ErrInvalid, bad)
	}
	u, err := ParseWithPrefix(New(PrefixRefund), PrefixRefund)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, u)
	_, err = ParseWithPrefix(New(PrefixRefund), PrefixPayment)
	require.ErrorIs(t, err, ErrInvalid)
	assert.True(t, HasPrefix("re_abc", PrefixRefund))
	assert.False(t, HasPrefix("ref_abc", PrefixRefund))
}

func TestNewUUIDIsV7(t *testing.T) {
	assert.Equal(t, uuid.Version(7), NewUUID().Version())
}

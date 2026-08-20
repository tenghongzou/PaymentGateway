package migrations

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSource(t *testing.T) {
	for _, svc := range Services {
		t.Run(svc, func(t *testing.T) {
			src, err := Source(svc)
			require.NoError(t, err)
			entries, err := fs.ReadDir(src, ".")
			require.NoError(t, err)
			var ups, downs int
			for _, e := range entries {
				switch {
				case strings.HasSuffix(e.Name(), ".up.sql"):
					ups++
				case strings.HasSuffix(e.Name(), ".down.sql"):
					downs++
				}
			}
			assert.Positive(t, ups)
			assert.Equal(t, ups, downs, "every up migration needs a down migration")
		})
	}
	_, err := Source("nope")
	require.Error(t, err)
}

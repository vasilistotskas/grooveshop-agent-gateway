package feeds

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestServesGzip(t *testing.T) {
	for _, tc := range []struct {
		header []string
		want   bool
	}{
		{header: nil, want: false},
		{header: []string{""}, want: false},
		{header: []string{"gzip"}, want: true},
		{header: []string{"gzip, deflate, br"}, want: true},
		// Unlisted identity has no weight to beat gzip's.
		{header: []string{"gzip;q=0.5"}, want: true},
		{header: []string{"gzip;q=0.001, br"}, want: true},
		{header: []string{"gzip;q=0.5, identity;q=0.8"}, want: false},
		{header: []string{"gzip;q=0.5, identity;q=0.5"}, want: true},
		{header: []string{"gzip;q=0.5, *;q=0.8"}, want: false},
		{header: []string{"*"}, want: true},
		{header: []string{"gzip;q=0, br"}, want: false},
		{header: []string{"br"}, want: false},
		// Neither acceptable: identity, never a refusal.
		{header: []string{"br, identity;q=0, *;q=0"}, want: false},
		{header: []string{"br", "gzip"}, want: true},
	} {
		assert.Equal(t, tc.want, servesGzip(tc.header), "%q", tc.header)
	}
}

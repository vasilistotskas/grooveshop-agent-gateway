package httphdr

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestETagMatches(t *testing.T) {
	const etag = `"abc"`
	for header, want := range map[string]bool{
		`"abc"`:              true,
		`W/"abc"`:            true,
		`"x", "abc"`:         true,
		`*`:                  true,
		`"other"`:            false,
		``:                   false,
		`W/"x" , W/"abc"   `: true,
		`"abcd"`:             false,
		`abc`:                false,
		`"ab`:                false,
		// An opaque-tag may hold a comma; it is one tag, not two.
		`"x,"abc"`:     false,
		`"a,b", "abc"`: true,
		`w/"abc"`:      false,
		`,,  "abc"`:    true,
		`"x" garbage`:  false,
		"\t\"abc\"\t":  true,
	} {
		assert.Equal(t, want, ETagMatches(header, etag), header)
	}
	// Weak comparison holds from the server's side too.
	assert.True(t, ETagMatches(`"abc"`, `W/"abc"`))
	assert.True(t, ETagMatches(`"a,b"`, `"a,b"`))
}

func TestEncodingWeight(t *testing.T) {
	cases := []struct {
		header         string
		gzip, identity float64
	}{
		{header: "", gzip: 0, identity: 1},
		{header: "gzip", gzip: 1, identity: 1},
		{header: "GZIP, deflate, br", gzip: 1, identity: 1},
		{header: "x-gzip", gzip: 1, identity: 1},
		{header: "gzip;q=0", gzip: 0, identity: 1},
		{header: "gzip; q=0.000", gzip: 0, identity: 1},
		{header: "gzip;Q=0.5, identity;q=0.8", gzip: 0.5, identity: 0.8},
		{header: "br", gzip: 0, identity: 1},
		{header: "*", gzip: 1, identity: 1},
		{header: "*;q=0", gzip: 0, identity: 0},
		{header: "*;q=0, identity", gzip: 0, identity: 1},
		{header: "gzip, *;q=0", gzip: 1, identity: 0},
		{header: "identity;q=0", gzip: 0, identity: 0},
		{header: "gzip;q=1.0, identity; q=0.5, *;q=0", gzip: 1, identity: 0.5},
		{header: "gzip;q=abc", gzip: 0, identity: 1},
		{header: "gzip;q=2", gzip: 0, identity: 1},
	}
	for _, tc := range cases {
		assert.InDelta(t, tc.gzip, EncodingWeight(tc.header, Gzip), 1e-9,
			"gzip in %q", tc.header)
		assert.InDelta(t, tc.identity, EncodingWeight(tc.header, Identity),
			1e-9, "identity in %q", tc.header)
	}
}

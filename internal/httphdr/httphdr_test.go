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
		` * `:                true,
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
		"\t\"abc\"\t":  true,
		// Malformed fields match nothing.
		`"x" garbage`:       false,
		`"abc"x`:            false,
		`"x" "abc"`:         false,
		`*junk`:             false,
		`*, "abc"`:          false,
		`"abc", *`:          false,
		`"a b", "abc"`:      false,
		"\"\x01\", \"abc\"": false,
	} {
		assert.Equal(t, want, ETagMatches([]string{header}, etag), header)
	}
	// Weak comparison holds from the server's side too.
	assert.True(t, ETagMatches([]string{`"abc"`}, `W/"abc"`))
	assert.True(t, ETagMatches([]string{`"a,b"`}, `"a,b"`))
	// Every line of the field is read.
	assert.True(t, ETagMatches([]string{`"x"`, `"y", "abc"`}, etag))
	assert.False(t, ETagMatches(nil, etag))
}

func TestEncodingWeight(t *testing.T) {
	cases := []struct {
		header         string
		gzip, identity float64
		gzipListed     bool
		identityListed bool
	}{
		{header: "", identity: 1},
		{header: "gzip", gzip: 1, gzipListed: true, identity: 1},
		{header: "GZIP, deflate, br", gzip: 1, gzipListed: true, identity: 1},
		{header: "x-gzip", gzip: 1, gzipListed: true, identity: 1},
		{header: "gzip;q=0", gzipListed: true, identity: 1},
		{header: "gzip; q=0.000", gzipListed: true, identity: 1},
		{header: "gzip;q=0.5", gzip: 0.5, gzipListed: true, identity: 1},
		{
			header: "gzip;Q=0.5, identity;q=0.8", gzip: 0.5, gzipListed: true,
			identity: 0.8, identityListed: true,
		},
		{header: "br", identity: 1},
		{
			header: "*", gzip: 1, gzipListed: true,
			identity: 1, identityListed: true,
		},
		{header: "*;q=0", gzipListed: true, identityListed: true},
		{header: "*;q=0, identity", gzipListed: true, identity: 1,
			identityListed: true},
		{header: "gzip, *;q=0", gzip: 1, gzipListed: true,
			identityListed: true},
		{header: "identity;q=0", identityListed: true},
		{
			header: "gzip;q=1.0, identity; q=0.5, *;q=0", gzip: 1,
			gzipListed: true, identity: 0.5, identityListed: true,
		},
		{header: "gzip;q=1.000", gzip: 1, gzipListed: true, identity: 1},
		{header: "gzip;q=0.123", gzip: 0.123, gzipListed: true, identity: 1},
		// Not RFC 9110 12.4.2 qvalues: read as 0.
		{header: "gzip;q=abc", gzipListed: true, identity: 1},
		{header: "gzip;q=2", gzipListed: true, identity: 1},
		{header: "gzip;q=1e-1", gzipListed: true, identity: 1},
		{header: "gzip;q=.5", gzipListed: true, identity: 1},
		{header: "gzip;q=+0.5", gzipListed: true, identity: 1},
		{header: "gzip;q=0x1p-1", gzipListed: true, identity: 1},
		{header: "gzip;q=0.1234", gzipListed: true, identity: 1},
		{header: "gzip;q=1.5", gzipListed: true, identity: 1},
		{header: "gzip;q", gzipListed: true, identity: 1},
		{header: "gzip; Q ", gzipListed: true, identity: 1},
		{header: "gzip;q=", gzipListed: true, identity: 1},
		// A parameter other than q leaves the weight at 1.
		{header: "gzip;level=9", gzip: 1, gzipListed: true, identity: 1},
	}
	for _, tc := range cases {
		gz, gzListed := EncodingWeight([]string{tc.header}, Gzip)
		assert.InDelta(t, tc.gzip, gz, 1e-9, "gzip in %q", tc.header)
		assert.Equal(t, tc.gzipListed, gzListed, "gzip listed in %q",
			tc.header)
		id, idListed := EncodingWeight([]string{tc.header}, Identity)
		assert.InDelta(t, tc.identity, id, 1e-9, "identity in %q", tc.header)
		assert.Equal(t, tc.identityListed, idListed,
			"identity listed in %q", tc.header)
	}
	// Every line of the field is read.
	gz, listed := EncodingWeight([]string{"br", "gzip;q=0.4"}, Gzip)
	assert.InDelta(t, 0.4, gz, 1e-9)
	assert.True(t, listed)
}

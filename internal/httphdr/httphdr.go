// Package httphdr reads the RFC 9110 request header fields that more
// than one handler negotiates on.
package httphdr

import (
	"strconv"
	"strings"
)

// ETagMatches reports whether an If-None-Match value names etag, by the
// weak comparison RFC 9110 13.1.2 requires (8.8.3.2: the opaque-tags
// match, "W/" on either side ignored). The value is "*", which matches
// any current representation, or a list of entity-tags. An opaque-tag
// may itself hold a comma, so the list is scanned by its quotes, never
// split on commas.
func ETagMatches(ifNoneMatch, etag string) bool {
	want := strings.TrimPrefix(etag, "W/")
	v := ifNoneMatch
	for {
		v = strings.TrimLeft(v, " \t,")
		if v == "" {
			return false
		}
		if v[0] == '*' {
			return true
		}
		v = strings.TrimPrefix(v, "W/")
		if v == "" || v[0] != '"' {
			return false
		}
		end := strings.IndexByte(v[1:], '"')
		if end < 0 {
			return false
		}
		if v[:end+2] == want {
			return true
		}
		v = v[end+2:]
	}
}

// Content codings and the Accept-Encoding tokens with a meaning of
// their own (RFC 9110 12.5.3).
const (
	Gzip     = "gzip"
	Identity = "identity"
	wildcard = "*"
	// RFC 9110 8.4.1.3: a recipient SHOULD treat x-gzip as gzip.
	xGzip = "x-gzip"
)

// EncodingWeight is the weight an Accept-Encoding value gives coding
// (RFC 9110 12.5.3, 12.4.2): its own entry's, else the "*" entry's,
// else 1 for identity, which is acceptable unless excluded, and 0 for
// any other coding, which is acceptable only when listed. A weight of 0
// means not acceptable; an unreadable one is read as 0. An absent or
// empty value therefore accepts identity alone.
func EncodingWeight(acceptEncoding, coding string) float64 {
	own, star := -1.0, -1.0
	for entry := range strings.SplitSeq(acceptEncoding, ",") {
		name, params, _ := strings.Cut(entry, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == xGzip {
			name = Gzip
		}
		switch name {
		case coding:
			own = weightOf(params)
		case wildcard:
			star = weightOf(params)
		}
	}
	switch {
	case own >= 0:
		return own
	case star >= 0:
		return star
	case coding == Identity:
		return 1
	}
	return 0
}

// weightOf reads the q parameter of one Accept-Encoding entry: 1 when
// absent, 0 when it is not a qvalue.
func weightOf(params string) float64 {
	for p := range strings.SplitSeq(params, ";") {
		key, val, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil || q < 0 || q > 1 {
			return 0
		}
		return q
	}
	return 1
}

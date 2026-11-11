// Package httphdr reads the RFC 9110 request header fields that more
// than one handler negotiates on. Each helper takes every line of its
// field (http.Header.Values), so no caller can read the first line only.
package httphdr

import (
	"regexp"
	"strconv"
	"strings"
)

// ETagMatches reports whether an If-None-Match field names etag, by the
// weak comparison RFC 9110 13.1.2 requires (8.8.3.2: the opaque-tags
// match, "W/" on either side ignored). The field is "*", which matches
// any current representation, or a list of entity-tags. An opaque-tag
// may itself hold a comma, so the list is scanned by its quotes, never
// split on commas. A malformed field matches nothing.
func ETagMatches(values []string, etag string) bool {
	field := strings.Trim(strings.Join(values, ","), " \t")
	if field == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	var matched bool
	for v := field; ; {
		v = strings.TrimLeft(v, " \t,")
		if v == "" {
			return matched
		}
		tag, rest, ok := entityTag(v)
		if !ok {
			return false
		}
		matched = matched || tag == want
		rest = strings.TrimLeft(rest, " \t")
		if rest != "" && rest[0] != ',' {
			return false
		}
		v = rest
	}
}

// entityTag splits the opaque-tag of the entity-tag at the start of v
// from what follows it (RFC 9110 8.8.3: [ "W/" ] DQUOTE *etagc DQUOTE).
func entityTag(v string) (tag, rest string, ok bool) {
	v = strings.TrimPrefix(v, "W/")
	if v == "" || v[0] != '"' {
		return "", "", false
	}
	for i := 1; i < len(v); i++ {
		switch c := v[i]; {
		case c == '"':
			return v[:i+1], v[i+1:], true
		case c == 0x21, c >= 0x23 && c <= 0x7E, c >= 0x80:
		default:
			return "", "", false
		}
	}
	return "", "", false
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

// EncodingWeight is the weight a present Accept-Encoding field gives
// coding (RFC 9110 12.5.3, 12.4.2), and whether the field lists it, by
// its own entry or by "*". Unlisted, identity weighs 1 (acceptable
// unless excluded) and any other coding 0 (acceptable only when
// listed). A weight of 0 means not acceptable; an entry whose weight is
// not a qvalue is read as 0. What an absent field means is the
// caller's to decide: any coding is acceptable then.
func EncodingWeight(values []string, coding string) (float64, bool) {
	own, star := -1.0, -1.0
	for _, line := range values {
		for entry := range strings.SplitSeq(line, ",") {
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
	}
	switch {
	case own >= 0:
		return own, true
	case star >= 0:
		return star, true
	case coding == Identity:
		return 1, false
	}
	return 0, false
}

// qvalue is RFC 9110 12.4.2's grammar: 0 or 1 with up to three decimals.
var qvalue = regexp.MustCompile(`^(?:0(?:\.[0-9]{0,3})?|1(?:\.0{0,3})?)$`)

// weightOf reads the q parameter of one Accept-Encoding entry: 1 when
// there is none, 0 when its value is not a qvalue, missing included.
func weightOf(params string) float64 {
	for p := range strings.SplitSeq(params, ";") {
		key, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		if !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		val = strings.TrimSpace(val)
		if !qvalue.MatchString(val) {
			return 0
		}
		q, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0
		}
		return q
	}
	return 1
}

package httpmw

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// These hosts sit behind Cloudflare, so X-Real-Ip carries the CF EDGE
// address: bucketing on it collapses every client behind one PoP into a
// single bucket — one aggressive agent then exhausts the limit for
// everyone sharing that edge, while a distributed scraper gets a fresh
// bucket per PoP. The storefront already resolves callers this way.
func TestClientIPPrefersTheRealCaller(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name: "cloudflare connecting ip wins over the edge hop",
			headers: map[string]string{
				"CF-Connecting-IP": "203.0.113.7",
				"X-Real-Ip":        "172.71.0.1",
				"X-Forwarded-For":  "203.0.113.7, 172.71.0.1",
			},
			want: "203.0.113.7",
		},
		{
			name:    "true-client-ip is honoured next",
			headers: map[string]string{"True-Client-IP": "203.0.113.8", "X-Real-Ip": "172.71.0.1"},
			want:    "203.0.113.8",
		},
		{
			name:    "forwarded-for falls back to the client-most entry",
			headers: map[string]string{"X-Forwarded-For": "203.0.113.9, 172.71.0.1", "X-Real-Ip": "172.71.0.1"},
			want:    "203.0.113.9",
		},
		{
			name:    "x-real-ip still used when nothing better exists",
			headers: map[string]string{"X-Real-Ip": "198.51.100.4"},
			want:    "198.51.100.4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			r.RemoteAddr = "10.42.0.1:5555"
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			assert.Equal(t, tc.want, clientIP(r))
		})
	}
}

func TestClientIPFallsBackToSocket(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r.RemoteAddr = "198.51.100.9:44321"
	assert.Equal(t, "198.51.100.9", clientIP(r))
}

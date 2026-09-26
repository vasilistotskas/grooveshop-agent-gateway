package httpmw

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Traefik keeps X-Forwarded-For only from a Cloudflare peer and then
// appends that peer, so the hop left of the last one is the visitor
// Cloudflare appended. The last hop alone is the CF EDGE: bucketing on it
// collapses every client behind one PoP into one bucket. And the headers
// Traefik does not strip must not decide the bucket, or a caller at a node
// IP picks a fresh one per request.
func TestClientIPPrefersTheRealCaller(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name: "through cloudflare: the visitor, not the edge hop",
			headers: map[string]string{
				"CF-Connecting-IP": "203.0.113.7",
				"X-Real-Ip":        "172.71.0.1",
				"X-Forwarded-For":  "203.0.113.7, 172.71.0.1",
			},
			want: "203.0.113.7",
		},
		{
			name: "entries a visitor added left of cloudflare's are ignored",
			headers: map[string]string{
				"X-Forwarded-For": "6.6.6.6, 203.0.113.9, 172.71.0.1",
			},
			want: "203.0.113.9",
		},
		{
			name: "direct caller: forged cloudflare headers do not pick the bucket",
			headers: map[string]string{
				"CF-Connecting-IP": "1.2.3.4",
				"True-Client-IP":   "1.2.3.5",
				"X-Real-Ip":        "198.51.100.4",
				"X-Forwarded-For":  "198.51.100.4",
			},
			want: "198.51.100.4",
		},
		{
			name:    "x-real-ip alone is not believed",
			headers: map[string]string{"X-Real-Ip": "198.51.100.4"},
			want:    "10.42.0.1",
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

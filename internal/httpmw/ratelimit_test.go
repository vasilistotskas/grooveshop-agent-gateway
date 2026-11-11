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

// An IPv6 caller is its /64, whatever address inside it a request comes
// from and however the address is written; an IPv4 caller is its address,
// written as IPv4-mapped IPv6 too.
func TestClientBucket(t *testing.T) {
	cases := []struct {
		name, hop, want string
	}{
		{"ipv4", "203.0.113.7", "203.0.113.7"},
		{"ipv4-mapped ipv6", "::ffff:203.0.113.7", "203.0.113.7"},
		{"ipv6", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"ipv6, another host of the /64", "2001:db8:1:2::9",
			"2001:db8:1:2::/64"},
		{"ipv6, expanded upper case", "2001:0DB8:0001:0002:FFFF:0:0:1",
			"2001:db8:1:2::/64"},
		{"ipv6, the next /64", "2001:db8:1:3::1", "2001:db8:1:3::/64"},
		{"not an address", "unknown", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			r.Header.Set("X-Forwarded-For", tc.hop+", 172.71.0.1")
			assert.Equal(t, tc.want, ClientBucket(r))
		})
	}
}

// The socket address is bucketed the same way.
func TestClientBucketOfTheSocket(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r.RemoteAddr = "[2001:db8::1]:44321"
	assert.Equal(t, "2001:db8::/64", ClientBucket(r))
}

// Requests from addresses of one IPv6 /64 spend one bucket; the next /64
// has its own.
func TestRateLimiterBucketsIPv6By64(t *testing.T) {
	rl := NewRateLimiter(60, 1, nil)
	h := rl.Middleware()(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	do := func(ip string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Forwarded-For", ip)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusNoContent, do("2001:db8:1:2::1"))
	assert.Equal(t, http.StatusTooManyRequests, do("2001:db8:1:2::2"))
	assert.Equal(t, http.StatusNoContent, do("2001:db8:1:3::1"))
}

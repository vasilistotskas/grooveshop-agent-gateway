package httpmw

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError + 1,
	}))
}

func TestRequestIDMintsAndEchoes(t *testing.T) {
	var seen string
	h := RequestID()(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = obs.RequestID(r.Context())
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.NotEmpty(t, seen)
	assert.Equal(t, seen, rec.Header().Get("X-Request-Id"))
}

func TestRequestIDHonorsInbound(t *testing.T) {
	cases := []struct{ name, inbound, want string }{
		{"kept", "req-123", "req-123"},
		// Django's rule (core/middleware/correlation_id.py): C0 controls
		// and DEL dropped, then 64 characters kept.
		{"controls dropped", "req\t-\x011\x7f23", "req-123"},
		{"capped", strings.Repeat("ab", 40), strings.Repeat("ab", 32)},
		{"capped in runes", strings.Repeat("α", 70), strings.Repeat("α", 64)},
		// Nothing left is no id: one is minted.
		{"emptied", "\x01\x02", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			h := RequestID()(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					seen = obs.RequestID(r.Context())
				}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Request-Id", tc.inbound)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if tc.want == "" {
				assert.NoError(t, uuid.Validate(seen))
			} else {
				assert.Equal(t, tc.want, seen)
			}
			assert.Equal(t, seen, rec.Header().Get("X-Request-Id"))
		})
	}
}

func TestRecoverConvertsPanicTo500(t *testing.T) {
	h := Recover(quietLogger())(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {
			panic("boom")
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRateLimiterRejectsBurstOverflow(t *testing.T) {
	rl := NewRateLimiter(60, 2, nil)
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

	assert.Equal(t, http.StatusNoContent, do("1.2.3.4"))
	assert.Equal(t, http.StatusNoContent, do("1.2.3.4"))
	assert.Equal(t, http.StatusTooManyRequests, do("1.2.3.4"))
	// A different client is unaffected.
	assert.Equal(t, http.StatusNoContent, do("5.6.7.8"))
}

func TestRateLimiterPartitionsPerTenantHost(t *testing.T) {
	// Agent platforms share egress IPs: exhausting tenant A's budget
	// from one IP must not consume tenant B's budget for the SAME IP —
	// buckets are keyed per (Host, IP).
	rl := NewRateLimiter(60, 1, nil)
	h := rl.Middleware()(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

	do := func(host string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		req.Header.Set("X-Forwarded-For", "9.9.9.9")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusNoContent, do("tenant-a.example"))
	assert.Equal(t, http.StatusTooManyRequests, do("tenant-a.example"))
	// Same IP, different tenant host: fresh bucket.
	assert.Equal(t, http.StatusNoContent, do("tenant-b.example"))
	// Host normalization: case and port do not split the bucket.
	assert.Equal(t, http.StatusTooManyRequests, do("TENANT-B.example:443"))
}

// The access line carries the tenant resolved inside the handler and the
// request id, both added by the logger's context handler.
func TestLoggingNamesTheRequestAndItsTenant(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(obs.NewContextHandler(slog.NewJSONHandler(&buf, nil)))
	h := RequestID()(Logging(log)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			obs.SetTenant(r.Context(), "demostore")
			w.WriteHeader(http.StatusNoContent)
		})))

	req := httptest.NewRequest(http.MethodGet, "/feeds/google.xml", nil)
	req.Header.Set("X-Request-Id", "req-1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.Equal(t, "request", line["msg"])
	assert.Equal(t, "req-1", line["request_id"])
	assert.Equal(t, "demostore", line["tenant"])
	assert.InDelta(t, http.StatusNoContent, line["status"], 0)
}

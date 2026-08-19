package httpmw

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
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
			seen = RequestIDFromContext(r.Context())
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.NotEmpty(t, seen)
	assert.Equal(t, seen, rec.Header().Get("X-Request-Id"))
}

func TestRequestIDHonorsInbound(t *testing.T) {
	var seen string
	h := RequestID()(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = RequestIDFromContext(r.Context())
		}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "req-123")
	h.ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, "req-123", seen)
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
		req.Header.Set("X-Real-Ip", ip)
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
		req.Header.Set("X-Real-Ip", "9.9.9.9")
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

func TestExtrasTenantEnrichment(t *testing.T) {
	var logged string
	h := Logging(quietLogger())(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			SetTenant(r.Context(), "webside")
			if e, ok := r.Context().Value(ctxKeyExtras{}).(*Extras); ok {
				logged = e.getTenant()
			}
		}))

	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "webside", logged)
}

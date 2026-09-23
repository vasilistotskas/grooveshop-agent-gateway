package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// A burst of order events from one Celery pod must all land: Django
// drops a 4xx, so a 429 here would lose the event for good.
func TestInternalRoutesBypassTheRateLimiter(t *testing.T) {
	limiter := httpmw.NewRateLimiter(1, 1, obs.NewMetrics())
	h := exceptInternal(limiter.Middleware(), http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

	status := func(path string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "10.42.0.7:5000"
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for range 10 {
		assert.Equal(t, http.StatusNoContent,
			status("/internal/events/order-status"))
	}
	assert.Equal(t, http.StatusNoContent, status("/mcp"))
	assert.Equal(t, http.StatusTooManyRequests, status("/mcp"))
}

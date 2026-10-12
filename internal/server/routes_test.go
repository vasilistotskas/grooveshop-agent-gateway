package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
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

// gateway is the full mux in front of a Django that knows two stores:
// shop.example.test sells to agents, off.example.test does not.
func gateway(t *testing.T) http.Handler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch domain := r.URL.Query().Get("domain"); domain {
			case "shop.example.test", "off.example.test":
				_, _ = w.Write([]byte(`{"schemaName": "demostore",` +
					`"storeName": "Demo Store", "primaryDomain": "` +
					domain + `", "agentCommerceEnabled": ` +
					strconv.FormatBool(domain == "shop.example.test") + `}`))
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail": "Store not found."}`))
			}
		}))
	t.Cleanup(upstream.Close)

	dj := django.New(upstream.URL+"/api/v1", "api.example.test", testSecret,
		time.Second, quietLog(), nil)
	rdb := unreachableRedis()
	metrics := obs.NewMetrics()
	return New(Deps{
		Cfg:     config.Config{RateLimitPerMin: 6000, RateLimitBurst: 1000},
		Log:     quietLog(),
		Metrics: metrics,
		Redis:   rdb,
		Django:  dj,
		Resolver: tenant.NewResolver(dj, rdb, time.Minute, time.Minute,
			quietLog(), metrics),
		Version: "test",
	})
}

func TestDiscoveryRoutes(t *testing.T) {
	h := gateway(t)
	call := func(method, host, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		return rec
	}
	offCard := call(http.MethodGet, "off.example.test", "/mcp/server-card")
	require.Equal(t, http.StatusNotFound, offCard.Code)

	for _, tc := range []struct{ path, contentType string }{
		{"/.well-known/ard.json", "application/json"},
		{"/.well-known/ai-catalog.json", "application/ai-catalog+json"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			on := call(http.MethodGet, "shop.example.test", tc.path)
			require.Equal(t, http.StatusOK, on.Code, on.Body.String())
			assert.Equal(t, tc.contentType, on.Header().Get("Content-Type"))
			assert.Contains(t, on.Body.String(),
				`"identifier":"urn:air:shop.example.test:mcp:store"`)
			assert.Equal(t, http.StatusNoContent,
				call(http.MethodOptions, "shop.example.test", tc.path).Code)

			// No MCP endpoint, nothing to describe: the Server Card's answer.
			off := call(http.MethodGet, "off.example.test", tc.path)
			assert.Equal(t, offCard.Code, off.Code)
			assert.Equal(t, offCard.Body.String(), off.Body.String())
			assert.JSONEq(t, `{"error":"not found"}`, off.Body.String())

			unknown := call(http.MethodGet, "unknown.example.test", tc.path)
			assert.Equal(t, http.StatusNotFound, unknown.Code)
			assert.JSONEq(t, `{"error":"unknown store"}`, unknown.Body.String())
		})
	}
}

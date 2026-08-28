package tenant

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiddlewareInjectsTenant(t *testing.T) {
	f := newFakeDjango(t)
	r := newTestResolver(t, f, time.Minute)

	var got *Tenant
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var ok bool
		got, ok = FromContext(req.Context())
		require.True(t, ok)
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
	req.Host = "shop.example.test"
	Middleware(r, testLogger())(next).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, got)
	assert.Equal(t, "demostore", got.SchemaName)
}

func TestMiddlewareUnknownHost404(t *testing.T) {
	f := newFakeDjango(t)
	r := newTestResolver(t, f, time.Minute)

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run for unknown hosts")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
	req.Host = "unknown.example.test"
	Middleware(r, testLogger())(next).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":"unknown store"}`, rec.Body.String())
}

func TestMiddlewareIgnoresForwardedHost(t *testing.T) {
	f := newFakeDjango(t)
	r := newTestResolver(t, f, time.Minute)

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("spoofed X-Forwarded-Host must not resolve a tenant")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
	req.Host = "unknown.example.test"
	// A client trying to pivot into another tenant's schema.
	req.Header.Set("X-Forwarded-Host", "shop.example.test")
	Middleware(r, testLogger())(next).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMiddlewareResolverError503(t *testing.T) {
	f := newFakeDjango(t)
	f.setFail(true)
	r := newTestResolver(t, f, time.Minute)

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run when resolution fails")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
	req.Host = "shop.example.test"
	Middleware(r, testLogger())(next).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestRequireAgentCommerce(t *testing.T) {
	on := true
	off := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name  string
		agent *bool
		want  int
	}{
		// nil = payload from an older Django or a stale cached
		// resolve — MUST fail open toward the pre-flag behavior.
		{"nil fails open", nil, http.StatusNoContent},
		{"explicit on", &on, http.StatusNoContent},
		{"explicit off 404s", &off, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tn := &Tenant{}
			tn.AgentCommerceEnabled = tc.agent
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
			req = req.WithContext(NewContext(req.Context(), tn))
			RequireAgentCommerce(next).ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}

	t.Run("missing tenant context 404s", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		RequireAgentCommerce(next).ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestRequireProductFeedsSubordinateToAgentGate(t *testing.T) {
	on := true
	off := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name  string
		agent *bool
		feeds *bool
		want  int
	}{
		{"both nil fails open", nil, nil, http.StatusNoContent},
		{"feeds off 404s", &on, &off, http.StatusNotFound},
		{"agent off kills feeds too", &off, &on, http.StatusNotFound},
		{"both on serves", &on, &on, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tn := &Tenant{}
			tn.AgentCommerceEnabled = tc.agent
			tn.ProductFeedsEnabled = tc.feeds
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
			req = req.WithContext(NewContext(req.Context(), tn))
			RequireProductFeeds(next).ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

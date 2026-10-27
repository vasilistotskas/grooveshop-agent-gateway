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
	f.goDown()
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
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name  string
		agent bool
		want  int
	}{
		{"on serves", true, http.StatusNoContent},
		// Off — or absent from the payload — is closed: a disabled
		// surface reads as a route that never existed.
		{"off 404s", false, http.StatusNotFound},
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
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name      string
		agent     bool
		feeds     bool
		catalogue bool
		want      int
	}{
		{"all off 404s", false, false, false, http.StatusNotFound},
		{"feeds off 404s", true, false, true, http.StatusNotFound},
		{"agent off kills feeds too", false, true, true, http.StatusNotFound},
		// A feed lists product pages, which a store without its
		// catalogue does not serve.
		{"catalogue off 404s", true, true, false, http.StatusNotFound},
		{"all on serves", true, true, true, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tn := &Tenant{}
			tn.AgentCommerceEnabled = tc.agent
			tn.ProductFeedsEnabled = tc.feeds
			tn.Features.Catalogue = tc.catalogue
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
			req = req.WithContext(NewContext(req.Context(), tn))
			RequireProductFeeds(next).ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestRequireCheckoutNeedsAgentCommerceAndTheCart(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name  string
		agent bool
		cart  bool
		want  int
	}{
		{"both off 404s", false, false, http.StatusNotFound},
		{"agent off 404s", false, true, http.StatusNotFound},
		// A store that takes no orders has nothing to check out.
		{"cart off 404s", true, false, http.StatusNotFound},
		{"both on serves", true, true, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tn := &Tenant{}
			tn.AgentCommerceEnabled = tc.agent
			tn.Features.Cart = tc.cart
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
			req = req.WithContext(NewContext(req.Context(), tn))
			RequireCheckout(next).ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

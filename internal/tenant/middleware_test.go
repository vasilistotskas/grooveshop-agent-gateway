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
	assert.Equal(t, "webside", got.SchemaName)
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

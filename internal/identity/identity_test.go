package identity

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

const profileJSON = `{"id":7,"email":"shopper@example.test",` +
	`"firstName":"Maria","lastName":"P"}`

// fakeDjango serves /agent/me: "good" → profile, "scopeless" → 403,
// anything else → 401. Counts upstream hits for cache assertions.
func fakeDjango(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/api/v1/agent/me", r.URL.Path)
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch r.Header.Get("Authorization") {
			case "Bearer good":
				_, _ = w.Write([]byte(profileJSON))
			case "Bearer scopeless":
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"detail":"missing scope"}`))
			default:
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"detail":"bad token"}`))
			}
		}))
}

func testTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName:    "webside",
			DefaultLocale: "el",
		},
		Domain: "shop.example.test",
	}
}

func newHarness(t *testing.T) (*Verifier, *atomic.Int64, func()) {
	t.Helper()
	var hits atomic.Int64
	srv := fakeDjango(t, &hits)
	dj := django.New(srv.URL+"/api/v1", "api.example.test", "s",
		2*time.Second, testLogger(), nil)
	return NewVerifier(dj, testLogger()), &hits, srv.Close
}

func TestVerifyValidTokenReturnsProfile(t *testing.T) {
	v, _, done := newHarness(t)
	defer done()

	linked, err := v.Verify(t.Context(), testTenant(), "good")
	require.NoError(t, err)
	require.NotNil(t, linked.Profile)
	assert.Equal(t, int64(7), linked.Profile.ID)
	assert.Equal(t, "good", linked.Bearer)
}

func TestVerifyInvalidToken(t *testing.T) {
	v, _, done := newHarness(t)
	defer done()

	_, err := v.Verify(t.Context(), testTenant(), "expired")
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyScopelessTokenPassesWithoutProfile(t *testing.T) {
	v, _, done := newHarness(t)
	defer done()

	linked, err := v.Verify(t.Context(), testTenant(), "scopeless")
	require.NoError(t, err)
	assert.Nil(t, linked.Profile)
	assert.Equal(t, "scopeless", linked.Bearer)
}

func TestVerifyCachesPositiveAndNegative(t *testing.T) {
	v, hits, done := newHarness(t)
	defer done()

	for range 3 {
		_, err := v.Verify(t.Context(), testTenant(), "good")
		require.NoError(t, err)
	}
	for range 3 {
		_, err := v.Verify(t.Context(), testTenant(), "expired")
		assert.ErrorIs(t, err, ErrInvalidToken)
	}
	// One upstream probe per distinct token — the rest served from cache.
	assert.Equal(t, int64(2), hits.Load())
}

func middlewareHarness(t *testing.T) (http.Handler, *atomic.Int64, func()) {
	t.Helper()
	v, hits, done := newHarness(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l, ok := FromContext(r.Context()); ok && l.Profile != nil {
			w.Header().Set("X-Linked-Email", l.Profile.Email)
		}
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(v, testLogger())(inner)
	// The identity middleware runs inside the tenant middleware; inject
	// the tenant context the same way.
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(
			tenant.NewContext(r.Context(), testTenant())))
	})
	return outer, hits, done
}

func TestMiddlewareAnonymousPassesThrough(t *testing.T) {
	h, hits, done := middlewareHarness(t)
	defer done()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int64(0), hits.Load())
}

func TestMiddlewareValidBearerAttachesIdentity(t *testing.T) {
	h, _, done := middlewareHarness(t)
	defer done()

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "shopper@example.test",
		rec.Header().Get("X-Linked-Email"))
}

func TestMiddlewareInvalidBearerGets401WithChallenge(t *testing.T) {
	h, _, done := middlewareHarness(t)
	defer done()

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer expired")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"),
		`resource_metadata="https://shop.example.test`+
			`/.well-known/oauth-protected-resource/mcp"`)
}

// otherTenant is a second store served by the same gateway pod pool —
// every tenant's ingress targets the same Service.
func otherTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName:    "acme",
			DefaultLocale: "en",
		},
		Domain: "acme.example.test",
	}
}

// A verdict is obtained per tenant, so it must not be reused across
// them. Keying the cache on the token alone let a bearer verified for
// one store be accepted at another with the first store's shopper
// profile attached.
func TestVerifyDoesNotReuseVerdictAcrossTenants(t *testing.T) {
	v, hits, done := newHarness(t)
	defer done()

	_, err := v.Verify(t.Context(), testTenant(), "good")
	require.NoError(t, err)
	require.Equal(t, int64(1), hits.Load())

	// Same token, different tenant: must re-probe rather than hit cache.
	_, err = v.Verify(t.Context(), otherTenant(), "good")
	require.NoError(t, err)
	assert.Equal(t, int64(2), hits.Load(),
		"second tenant must be verified upstream, not served from cache")

	// Same tenant again still caches.
	_, err = v.Verify(t.Context(), testTenant(), "good")
	require.NoError(t, err)
	assert.Equal(t, int64(2), hits.Load(), "same-tenant repeat should hit cache")
}

// The negative direction: a token rejected at one store must not mark
// it invalid at its own store for the rest of the TTL.
func TestVerifyDoesNotReuseRejectionAcrossTenants(t *testing.T) {
	v, hits, done := newHarness(t)
	defer done()

	_, err := v.Verify(t.Context(), testTenant(), "nope")
	require.Error(t, err)
	require.Equal(t, int64(1), hits.Load())

	_, err = v.Verify(t.Context(), otherTenant(), "nope")
	require.Error(t, err)
	assert.Equal(t, int64(2), hits.Load(),
		"rejection for one tenant must not answer for another")
}

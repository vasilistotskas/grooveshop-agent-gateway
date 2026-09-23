package ucp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

// platformProfile is the recorded platform profile fixture — minimal and
// valid against the spec's platform_schema — with its order webhook
// endpoint filled in.
func platformProfileJSON(t *testing.T, webhookURL string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "ucp", "platform_profile.json"))
	require.NoError(t, err)
	return strings.ReplaceAll(string(raw), "{{webhook_url}}", webhookURL)
}

type profileServer struct {
	*httptest.Server
	hits atomic.Int32
}

func serveProfile(
	t *testing.T, handler func(http.ResponseWriter, *http.Request),
) *profileServer {
	t.Helper()
	s := &profileServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			s.hits.Add(1)
			handler(w, r)
		}))
	t.Cleanup(s.Close)
	return s
}

func newTestResolver(t *testing.T) *ProfileResolver {
	t.Helper()
	r, err := NewProfileResolver(true)
	require.NoError(t, err)
	return r
}

func discoveryCode(t *testing.T, err error) string {
	t.Helper()
	derr, ok := errors.AsType[*DiscoveryError](err)
	require.True(t, ok, "want a DiscoveryError, got %v", err)
	return derr.Code
}

func TestResolveValidatesAndCachesTheProfile(t *testing.T) {
	body := platformProfileJSON(t, "https://p.example/hooks")
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	r := newTestResolver(t)
	ctx := context.Background()

	p, err := r.Resolve(ctx, srv.URL+"/profile.json")
	require.NoError(t, err)
	require.NoError(t, CheckVersion(p))
	n := Negotiate(BusinessCapabilities(testTenant()), p)
	assert.True(t, n.Has(CapabilityCheckout))
	assert.Equal(t, "https://p.example/hooks", n.OrderWebhookURL())

	_, err = r.Resolve(ctx, srv.URL+"/profile.json")
	require.NoError(t, err)
	assert.EqualValues(t, 1, srv.hits.Load(), "cached for at least 60s")
}

// A failed discovery is cached too, so a broken or hostile endpoint is
// not fetched on every request.
func TestResolveBacksOffAFailingProfile(t *testing.T) {
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	r := newTestResolver(t)
	for range 3 {
		_, err := r.Resolve(context.Background(), srv.URL)
		assert.Equal(t, CodeProfileUnreachable, discoveryCode(t, err))
	}
	assert.EqualValues(t, 1, srv.hits.Load())
}

func TestResolveRejectsWhatTheSpecRejects(t *testing.T) {
	cases := map[string]struct {
		handler func(http.ResponseWriter, *http.Request)
		code    string
	}{
		"redirect is not followed": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}, CodeProfileUnreachable},
		"not json": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>"))
		}, CodeProfileMalformed},
		"violates the platform schema": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ucp":{"version":"` + Version + `"}}`))
		}, CodeProfileMalformed},
		"oversized": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"pad":"` +
				strings.Repeat("x", maxProfileBytes) + `"}`))
		}, CodeProfileMalformed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := serveProfile(t, tc.handler)
			_, err := newTestResolver(t).Resolve(context.Background(), srv.URL)
			assert.Equal(t, tc.code, discoveryCode(t, err))
		})
	}
}

// Production resolves https public profiles only.
func TestResolveRefusesUnsafeProfileURLs(t *testing.T) {
	r, err := NewProfileResolver(false)
	require.NoError(t, err)
	for _, raw := range []string{
		"http://platform.example/profile.json",
		"https://127.0.0.1/profile.json",
		"https://ucp.dev@evil.example/profile.json",
		"",
	} {
		_, err := r.Resolve(context.Background(), raw)
		assert.Equal(t, CodeInvalidProfileURL, discoveryCode(t, err), raw)
	}
}

func TestCheckVersion(t *testing.T) {
	var p PlatformProfile
	p.UCP.Version = "2026-01-23"
	assert.Equal(t, CodeVersionUnsupported, discoveryCode(t, CheckVersion(&p)))
	p.UCP.Version = Version
	assert.NoError(t, CheckVersion(&p))
}

func TestProfileTTL(t *testing.T) {
	for header, want := range map[string]time.Duration{
		"":                         profileTTLFloor,
		"no-store":                 profileTTLFloor,
		"public, max-age=10":       profileTTLFloor,
		"public, max-age=600":      10 * time.Minute,
		`max-age="120"`:            2 * time.Minute,
		"max-age=86400, immutable": profileTTLCeiling,
	} {
		assert.Equal(t, want, profileTTL(header), header)
	}
}

// A spent discovery budget is the gateway's condition, not the URL's: it
// is answered as retryable and never cached against the profile.
func TestResolveBudgetExhaustionIsNotCached(t *testing.T) {
	body := platformProfileJSON(t, "https://p.example/hooks")
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	r := newTestResolver(t)
	r.limiter = rate.NewLimiter(0, 0)
	_, err := r.Resolve(context.Background(), srv.URL)
	assert.ErrorIs(t, err, ErrDiscoveryBusy)

	r.limiter = rate.NewLimiter(rate.Inf, 1)
	_, err = r.Resolve(context.Background(), srv.URL)
	assert.NoError(t, err, "the busy answer was not remembered")
}

// Error content reaches anonymous callers: a transport failure must not
// echo the dialled address.
func TestResolveHidesTransportErrorDetail(t *testing.T) {
	r, err := NewProfileResolver(false)
	require.NoError(t, err)
	r.hc = platformClient(false, time.Second)
	// A public-looking name that resolves nowhere public in tests.
	_, err = r.Resolve(context.Background(),
		"https://profile.invalid/profile.json")
	derr, ok := errors.AsType[*DiscoveryError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, "unable to fetch the platform profile", derr.Content)
}

// Eviction removes the least recently used entry, so a flood of one-off
// URLs cannot push out a platform that keeps calling.
func TestProfileCacheEvictsLeastRecentlyUsed(t *testing.T) {
	r := newTestResolver(t)
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	for i := range maxProfiles {
		// Distinct recency per entry: the clock is too coarse to order
		// entries written in one loop.
		r.cache[strconv.Itoa(i)] = profileEntry{
			profile: &PlatformProfile{}, expires: future,
			used: past.Add(time.Duration(i) * time.Second),
		}
	}
	_, _, ok := r.cached("0") // the busy platform keeps calling
	require.True(t, ok)
	r.remember("flood", profileEntry{profile: &PlatformProfile{}, expires: future})
	_, _, ok = r.cached("0")
	assert.True(t, ok, "a recently used entry survives")
	_, _, ok = r.cached("1")
	assert.False(t, ok, "the least recently used entry went")
}

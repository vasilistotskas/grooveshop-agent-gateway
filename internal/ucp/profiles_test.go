package ucp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
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

// Staging only fetches a platform profile over public https, so the agent
// protocols runbook points at this file in the public repository. It names
// no webhook: staging has no public endpoint to receive one.
func TestRunbookPlatformProfileIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "ucp", "runbook_platform_profile.json"))
	require.NoError(t, err)
	schema, err := CompileSpec("profile.json#/$defs/platform_schema")
	require.NoError(t, err)
	var doc any
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NoError(t, schema.Validate(doc))

	var profile PlatformProfile
	require.NoError(t, json.Unmarshal(raw, &profile))
	assert.Equal(t, Version, profile.UCP.Version)
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

// callers names a new caller for each context caller makes.
var callers atomic.Int64

// caller is the test's context, as the rate limiter's middleware leaves
// it for a request (httpmw.ClientFrom), from a caller of its own: a test
// of anything but the caller's share fetches within that share.
func caller(t *testing.T) context.Context {
	t.Helper()
	return httpmw.WithClient(t.Context(),
		fmt.Sprintf("caller-%d", callers.Add(1)))
}

func newTestResolver(t *testing.T) *ProfileResolver {
	t.Helper()
	r, err := NewProfileResolver(true, slog.New(slog.DiscardHandler))
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
	ctx := caller(t)

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
		_, err := r.Resolve(caller(t), srv.URL)
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
			_, err := newTestResolver(t).Resolve(caller(t), srv.URL)
			assert.Equal(t, tc.code, discoveryCode(t, err))
		})
	}
}

// Production resolves https public profiles only.
func TestResolveRefusesUnsafeProfileURLs(t *testing.T) {
	r, err := NewProfileResolver(false, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	for _, raw := range []string{
		"http://platform.example/profile.json",
		"https://127.0.0.1/profile.json",
		"https://ucp.dev@evil.example/profile.json",
		"",
	} {
		_, err := r.Resolve(caller(t), raw)
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
	_, err := r.Resolve(caller(t), srv.URL)
	require.ErrorIs(t, err, ErrDiscoveryBusy)

	r.limiter = rate.NewLimiter(rate.Inf, 1)
	_, err = r.Resolve(caller(t), srv.URL)
	assert.NoError(t, err, "the busy answer was not remembered")
}

// One caller fetches at most its share of the discovery budget: past its
// burst, a profile it names is busy for it, told to come back when its
// share has a token again, and spends none of the shared budget, while
// another caller still fetches; one it fetched is served from the cache
// whatever its share.
func TestACallerFetchesOnlyItsShareOfTheBudget(t *testing.T) {
	body := platformProfileJSON(t, "https://p.example/hooks")
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	r := newTestResolver(t)
	// A shared budget that does not refill, so what it holds is exact.
	r.limiter = rate.NewLimiter(0, fetchBurst)
	flooding := httpmw.WithClient(t.Context(), "198.51.100.7")
	for i := range callerFetchBurst {
		_, err := r.Resolve(flooding, fmt.Sprintf("%s/p%d.json", srv.URL, i))
		require.NoError(t, err, i)
	}
	shared := r.limiter.Tokens()
	require.InDelta(t, float64(fetchBurst-callerFetchBurst), shared, 1e-9)
	_, err := r.Resolve(flooding, srv.URL+"/one-more.json")
	require.ErrorIs(t, err, ErrDiscoveryBusy)
	assert.InDelta(t, shared, r.limiter.Tokens(), 1e-9,
		"the shared budget is not spent on it")
	// The share refills one fetch every 60/callerFetchesPerMin seconds.
	assert.Equal(t, 60/callerFetchesPerMin, DiscoveryRetryAfter(err))

	_, err = r.Resolve(flooding, srv.URL+"/p0.json")
	require.NoError(t, err, "cached")
	_, err = r.Resolve(httpmw.WithClient(t.Context(), "2001:db8:1:2::/64"),
		srv.URL+"/one-more.json")
	require.NoError(t, err, "another caller")
}

// A spent shared budget spends none of a caller's share: once it refills,
// the caller fetches its whole burst.
func TestASpentSharedBudgetSpendsNoCallersShare(t *testing.T) {
	body := platformProfileJSON(t, "https://p.example/hooks")
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	r := newTestResolver(t)
	r.limiter = rate.NewLimiter(fetchRate, fetchBurst)
	require.True(t, r.limiter.AllowN(time.Now(), fetchBurst))
	caller := httpmw.WithClient(t.Context(), "198.51.100.7")
	for i := range 2 * callerFetchBurst {
		_, err := r.Resolve(caller, fmt.Sprintf("%s/busy%d.json", srv.URL, i))
		require.ErrorIs(t, err, ErrDiscoveryBusy, i)
		assert.Equal(t, 1, DiscoveryRetryAfter(err),
			"the shared budget refills within a second")
	}

	r.limiter = rate.NewLimiter(rate.Inf, 1)
	for i := range callerFetchBurst {
		_, err := r.Resolve(caller, fmt.Sprintf("%s/p%d.json", srv.URL, i))
		require.NoError(t, err, "the share is whole: %d", i)
	}
}

// Every caller is charged its own share before it fetches or waits on a
// fetch of the same URL: a caller past its share is refused even while
// another caller's fetch of that URL is under way, a caller with share
// left that joins one gets the profile, charged for it, and the caller
// that started it waits on it again for nothing.
func TestEachCallerOfOneFetchIsChargedItsOwnShare(t *testing.T) {
	body := platformProfileJSON(t, "https://p.example/hooks")
	release := make(chan struct{})
	asked := make(chan struct{}, 8)
	srv := serveProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/held.json" {
			asked <- struct{}{}
			<-release
		}
		_, _ = w.Write([]byte(body))
	})
	// Released at the latest when the test ends, before the server
	// closes, so a failing test does not hang on the held fetch.
	var once sync.Once
	unhold := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unhold)
	r := newTestResolver(t)
	spent := httpmw.WithClient(t.Context(), "198.51.100.7")
	for i := range callerFetchBurst {
		_, err := r.Resolve(spent, fmt.Sprintf("%s/p%d.json", srv.URL, i))
		require.NoError(t, err, i)
	}
	leader := httpmw.WithClient(t.Context(), "198.51.100.8")
	joiner := httpmw.WithClient(t.Context(), "198.51.100.9")
	type result struct {
		p   *PlatformProfile
		err error
	}
	resolve := func(ctx context.Context) <-chan result {
		out := make(chan result, 1)
		go func() {
			p, err := r.Resolve(ctx, srv.URL+"/held.json")
			out <- result{p, err}
		}()
		return out
	}
	led := resolve(leader)
	<-asked

	select {
	case res := <-resolve(spent):
		require.ErrorIs(t, res.err, ErrDiscoveryBusy,
			"refused, not carried by the fetch under way")
	case <-time.After(5 * time.Second):
		require.Fail(t, "a caller past its share joined the fetch")
	}
	joined := resolve(joiner)
	again := resolve(leader)
	unhold()
	for _, out := range []<-chan result{led, joined, again} {
		res := <-out
		require.NoError(t, res.err)
		assert.NotNil(t, res.p)
	}
	assert.EqualValues(t, callerFetchBurst+1, srv.hits.Load(), "one fetch")

	// Each paid one token for the fetch: burst-1 fetches are left.
	for name, ctx := range map[string]context.Context{
		"joiner": joiner, "leader": leader,
	} {
		for i := range callerFetchBurst - 1 {
			_, err := r.Resolve(ctx,
				fmt.Sprintf("%s/%s%d.json", srv.URL, name, i))
			require.NoError(t, err, "%s %d", name, i)
		}
		_, err := r.Resolve(ctx, srv.URL+"/"+name+"-last.json")
		require.ErrorIs(t, err, ErrDiscoveryBusy, name)
	}
}

// A context no rate limiter named the caller of fetches nothing: every
// fetch is a caller's, never a bucket shared by every unnamed one.
func TestAFetchNeedsANamedCaller(t *testing.T) {
	srv := serveProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(platformProfileJSON(t, "https://p.example/h")))
	})
	r := newTestResolver(t)
	_, err := r.Resolve(t.Context(), srv.URL+"/profile.json")
	require.ErrorIs(t, err, errNoCaller)
	require.NotErrorIs(t, err, ErrDiscoveryBusy)
	_, err = r.Refresh(t.Context(), srv.URL+"/profile.json")
	require.ErrorIs(t, err, errNoCaller)
	assert.Zero(t, srv.hits.Load())
}

// A panic in a fetch comes back as an error, without the panic text or
// stack, since a tool result carries it to the agent, instead of ending
// the process from singleflight's own goroutine.
func TestResolveReturnsPanicAsError(t *testing.T) {
	r := newTestResolver(t)
	r.hc = nil // sending the request panics
	_, err := r.Resolve(caller(t),
		"http://127.0.0.1:1/profile.json")
	require.EqualError(t, err, "ucp: profile fetch: panicked")
}

// Error content reaches anonymous callers: a transport failure must not
// echo the dialled address.
func TestResolveHidesTransportErrorDetail(t *testing.T) {
	r, err := NewProfileResolver(false, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	r.hc = platformClient(false, time.Second)
	// A public-looking name that resolves nowhere public in tests.
	_, err = r.Resolve(caller(t),
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

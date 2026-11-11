package tenant

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

const secretsHost = "shop.example.test"

// serve runs one request for secretsHost through mw and returns the
// response and the tenant the handler saw (nil when it did not run).
func serve(mw func(http.Handler) http.Handler) (*httptest.ResponseRecorder, *Tenant) {
	var seen *Tenant
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen, _ = FromContext(req.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
	req.Host = secretsHost
	mw(next).ServeHTTP(rec, req)
	return rec, seen
}

// twoPods returns pod A, which resolved the store from Django and wrote the
// shared Redis, and pod B, which has only what Redis holds: a tenant with no
// credentials.
func twoPods(
	t *testing.T, f *fakeDjango, rd *fakeRedis,
) (b *Resolver, fromRedis *Tenant) {
	t.Helper()
	a := newPod(t, f, rd.client())
	_, err := a.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	b = newPod(t, f, rd.client())
	fromRedis, err = b.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.calls.Load(), "pod B must read Redis")
	return b, fromRedis
}

func TestSecretsStayOutOfRedisAndAreToppedUp(t *testing.T) {
	f := newFakeDjango(t)
	rd := newFakeRedis(t)
	podA := newPod(t, f, rd.client())

	a, err := podA.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	assert.True(t, a.SecretsLoaded)
	assert.Equal(t, testChatKey, a.ChatAPIKey)

	raw, ok := rd.raw(redisKey(secretsHost))
	require.True(t, ok, "the config must be cached in Redis")
	assert.Contains(t, raw, `"schemaName":"demostore"`)
	assert.NotContains(t, raw, testChatKey)
	assert.NotContains(t, raw, testACPBearer)

	podB, b := twoPods(t, f, rd)
	assert.False(t, b.SecretsLoaded)
	assert.Empty(t, b.ChatAPIKey)
	assert.Empty(t, b.ACPBearerToken)

	full, err := podB.EnsureSecrets(t.Context(), b)
	require.NoError(t, err)
	assert.True(t, full.SecretsLoaded)
	assert.Equal(t, testChatKey, full.ChatAPIKey)
	assert.Equal(t, testACPBearer, full.ACPBearerToken)
	assert.Equal(t, "demostore", full.SchemaName)
	assert.Equal(t, secretsHost, full.Domain)
	assert.Equal(t, int32(2), f.calls.Load(), "one Django call per top-up")

	// The memory tier now holds the full tenant: neither a resolve nor
	// a second top-up goes back to Django.
	again, err := podB.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	assert.Same(t, full, again)
	same, err := podB.EnsureSecrets(t.Context(), again)
	require.NoError(t, err)
	assert.Same(t, full, same)
	assert.Equal(t, int32(2), f.calls.Load())

	// And the credentials still never reached Redis.
	raw, ok = rd.raw(redisKey(secretsHost))
	require.True(t, ok)
	assert.NotContains(t, raw, testChatKey)
	assert.NotContains(t, raw, testACPBearer)
}

func TestEnsureSecretsWithoutATenantIsANoOp(t *testing.T) {
	f := newFakeDjango(t)
	r := newTestResolver(t, f)

	got, err := r.EnsureSecrets(t.Context(), nil)
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Zero(t, f.calls.Load())
}

func TestMiddlewareWithSecretsServesTheToppedUpTenant(t *testing.T) {
	f := newFakeDjango(t)
	podB, _ := twoPods(t, f, newFakeRedis(t))

	// Middleware, for the routes that need no credential, serves a
	// Redis-tier tenant as it is and spends no Django call.
	rec, seen := serve(Middleware(podB, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.False(t, seen.SecretsLoaded)
	assert.Equal(t, int32(1), f.calls.Load())

	rec, seen = serve(MiddlewareWithSecrets(podB, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.True(t, seen.SecretsLoaded)
	assert.Equal(t, testChatKey, seen.ChatAPIKey)
	assert.Equal(t, testACPBearer, seen.ACPBearerToken)
	assert.Equal(t, int32(2), f.calls.Load())
}

// A failed top-up answers the caller with the error and caches nothing:
// the tenant in memory stays the public one, a later top-up asks Django
// again, and the routes that need no credential keep serving.
func TestEnsureSecretsFailureCachesNothing(t *testing.T) {
	f := newFakeDjango(t)
	podB, b := twoPods(t, f, newFakeRedis(t))
	f.goDown()
	before := f.calls.Load()

	_, err := podB.EnsureSecrets(t.Context(), b)
	require.ErrorIs(t, err, django.ErrUpstreamDown)
	assert.NotErrorIs(t, err, ErrUnknownTenant)
	assert.Greater(t, f.calls.Load(), before)

	rec, seen := serve(MiddlewareWithSecrets(podB, testLogger()))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Nil(t, seen, "the handler must not run without its credential")

	// Memory is untouched: not stale, not negative, still without secrets.
	resolved := f.calls.Load()
	got, err := podB.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	assert.False(t, got.SecretsLoaded)
	assert.False(t, got.Stale)
	rec, _ = serve(Middleware(podB, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, resolved, f.calls.Load())

	// No failure is remembered: once Django is back the next request
	// tops up.
	f.comeUp()
	full, err := podB.EnsureSecrets(t.Context(), b)
	require.NoError(t, err)
	assert.Equal(t, testChatKey, full.ChatAPIKey)
}

// waitingOnFlight counts the goroutines parked in Resolver.shared's select,
// which is where a caller sits once it has joined a flight. Nothing else
// exposes that.
func waitingOnFlight() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, "(*Resolver).shared(") &&
			strings.Contains(g, " [select]:") {
			n++
		}
	}
	return n
}

// Callers that overlap on one host share one Django call.
func TestEnsureSecretsCollapsesConcurrentTopUps(t *testing.T) {
	f := newFakeDjango(t)
	podB, b := twoPods(t, f, newFakeRedis(t))

	hold := make(chan struct{})
	f.mu.Lock()
	f.hold = hold
	f.mu.Unlock()
	before := f.calls.Load()

	const callers = 20
	var done sync.WaitGroup
	for range callers {
		done.Go(func() {
			full, err := podB.EnsureSecrets(t.Context(), b)
			if assert.NoError(t, err) {
				assert.Equal(t, testChatKey, full.ChatAPIKey)
			}
		})
	}
	// Release Django only once every caller waits on the flight, so a
	// late one cannot start a second.
	require.Eventually(t, func() bool {
		return waitingOnFlight() == callers
	}, 5*time.Second, time.Millisecond)
	close(hold)
	done.Wait()

	assert.Equal(t, int32(1), f.calls.Load()-before,
		"overlapping top-ups must share one Django call")
}

// The store was switched off after the caches were filled. Django answers
// 404 for it, so a resolve that reaches Django is a 404 here, but until
// the cached entry expires a store is still served. The routes that need
// the credentials do reach Django, and ask it again on every request:
// they answer 503, the outage answer, not 404.
//
// This pins what the code does today. It is under review (issue #91):
// EnsureSecrets could treat django.ErrNotFound as ErrUnknownTenant, as
// resolveSlow does.
func TestEnsureSecretsForADeactivatedStore(t *testing.T) {
	f := newFakeDjango(t)
	podB, b := twoPods(t, f, newFakeRedis(t))
	f.deactivate()

	_, err := podB.EnsureSecrets(t.Context(), b)
	require.ErrorIs(t, err, django.ErrNotFound)
	assert.NotErrorIs(t, err, ErrUnknownTenant)

	// Nothing marks the store unknown: Django is asked again each time.
	before := f.calls.Load()
	for range 2 {
		rec, seen := serve(MiddlewareWithSecrets(podB, testLogger()))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Nil(t, seen)
	}
	assert.Equal(t, before+2, f.calls.Load())

	// Routes that need no credential serve it from the cache.
	rec, seen := serve(Middleware(podB, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.Equal(t, "demostore", seen.SchemaName)

	// A pod with no cache of the store gets the 404 on both.
	cold := newPod(t, f, newFakeRedis(t).client())
	for _, mw := range []func(http.Handler) http.Handler{
		Middleware(cold, testLogger()),
		MiddlewareWithSecrets(cold, testLogger()),
	} {
		rec, seen := serve(mw)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Nil(t, seen)
	}
}

// When a pod's memory entry expires while Redis still holds the config
// (another pod, or this one, wrote it within the TTL), fromRedis replaces
// the entry, which had the credentials, with one that has none. The next
// /chat or /acp request tops it up: one Django call, which a pod with no
// Redis tier would have made too.
//
// While Django is down the two differ. With no Redis tier the pod serves
// its expired entry as stale, credentials included. With one, the Redis
// hit stands in for the stale answer and the top-up has no stale
// fallback, so /chat and /acp answer 503.
//
// This pins what the code does today. It is under review (issue #91).
func TestRedisHitReplacesAnExpiredEntryThatHadSecrets(t *testing.T) {
	f := newFakeDjango(t)
	withRedis := newPod(t, f, newFakeRedis(t).client())
	noRedis := newPod(t, f, unreachableRedis())
	for _, r := range []*Resolver{withRedis, noRedis} {
		got, err := r.Resolve(t.Context(), secretsHost)
		require.NoError(t, err)
		require.True(t, got.SecretsLoaded)
	}
	require.Equal(t, int32(2), f.calls.Load())

	expireMemory(withRedis)
	expireMemory(noRedis)

	got, err := withRedis.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	assert.False(t, got.SecretsLoaded, "the Redis hit lost the credentials")
	assert.Empty(t, got.ChatAPIKey)
	assert.Equal(t, int32(2), f.calls.Load(), "Redis answered; no Django call")

	f.goDown()

	rec, seen := serve(MiddlewareWithSecrets(withRedis, testLogger()))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Nil(t, seen)

	rec, seen = serve(MiddlewareWithSecrets(noRedis, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.True(t, seen.Stale)
	assert.Equal(t, testChatKey, seen.ChatAPIKey)
}

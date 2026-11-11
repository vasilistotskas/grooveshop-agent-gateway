package tenant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
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
) (a, b *Resolver, fromRedis *Tenant) {
	t.Helper()
	a = newPod(t, f, rd.client())
	_, err := a.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	b = newPod(t, f, rd.client())
	fromRedis, err = b.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.calls.Load(), "pod B must read Redis")
	return a, b, fromRedis
}

func TestSecretsStayOutOfRedisAndAreToppedUp(t *testing.T) {
	f := newFakeDjango(t)
	rd := newFakeRedis(t)
	var logs capturedLog
	podA := newPodLogging(t, f, rd.client(), logs.logger())

	// A cold resolve: a Redis miss, then Django.
	a, err := podA.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	assert.True(t, a.SecretsLoaded)
	assert.Equal(t, testChatKey, a.ChatAPIKey)
	assertMemoryTTL(t, podA, secretsHost, testTTL)

	raw, ok := rd.raw(redisKey(secretsHost))
	require.True(t, ok, "the config must be cached in Redis")
	assert.Contains(t, raw, `"schemaName":"demostore"`)
	assert.NotContains(t, raw, testChatKey)
	assert.NotContains(t, raw, testACPBearer)

	podB := newPodLogging(t, f, rd.client(), logs.logger())
	b, err := podB.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.calls.Load(), "pod B must read Redis")
	assert.False(t, b.SecretsLoaded)
	assert.Empty(t, b.ChatAPIKey)
	assert.Empty(t, b.ACPBearerToken)
	assertMemoryTTL(t, podB, secretsHost, testTTL)

	full, err := podB.EnsureSecrets(t.Context(), b)
	require.NoError(t, err)
	assert.True(t, full.SecretsLoaded)
	assert.Equal(t, testChatKey, full.ChatAPIKey)
	assert.Equal(t, testACPBearer, full.ACPBearerToken)
	assert.Equal(t, "demostore", full.SchemaName)
	assert.Equal(t, secretsHost, full.Domain)
	assert.Equal(t, int32(2), f.calls.Load(), "one Django call per top-up")
	assertMemoryTTL(t, podB, secretsHost, testTTL)

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

	// A Redis miss and a Redis hit are both silent: neither is the
	// "entry corrupt" or "read failed" warning.
	assert.Empty(t, logs.String())
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
	_, podB, _ := twoPods(t, f, newFakeRedis(t))

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

// A failed top-up answers the caller with the error and leaves the tenant
// in memory as it was: public, not stale, not negative, and the routes that
// need no credential keep serving it. The failure is remembered for the
// negative TTL, as a stale answer is, so a Django that hangs is not asked
// by every credential request; after it the next top-up asks again.
func TestAFailedTopUpIsRememberedForTheNegativeTTL(t *testing.T) {
	f := newFakeDjango(t)
	_, podB, b := twoPods(t, f, newFakeRedis(t))
	f.goDown()
	before := f.calls.Load()

	_, err := podB.EnsureSecrets(t.Context(), b)
	require.ErrorIs(t, err, django.ErrUpstreamDown)
	assert.NotErrorIs(t, err, ErrUnknownTenant)
	assert.Greater(t, f.calls.Load(), before)
	assertTopUpTTL(t, podB, testNegTTL)

	asked := f.calls.Load()
	rec, seen := serve(MiddlewareWithSecrets(podB, testLogger()))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Nil(t, seen, "the handler must not run without its credential")
	assert.Equal(t, asked, f.calls.Load(), "the failure is remembered")

	got, err := podB.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	assert.False(t, got.SecretsLoaded)
	assert.False(t, got.Stale)
	rec, _ = serve(Middleware(podB, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, asked, f.calls.Load())

	f.comeUp()
	expireMemory(t, podB, secretsHost)
	rec, seen = serve(MiddlewareWithSecrets(podB, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.Equal(t, testChatKey, seen.ChatAPIKey)
}

// assertTopUpTTL checks that the top-up remembered for secretsHost expires
// ttl from now.
func assertTopUpTTL(t *testing.T, r *Resolver, ttl time.Duration) {
	t.Helper()
	r.mu.RLock()
	e, ok := r.mem[secretsHost]
	r.mu.RUnlock()
	require.True(t, ok, "no memory entry for %s", secretsHost)
	assert.WithinDuration(t, time.Now().Add(ttl), e.topUp.until, time.Second,
		"the remembered top-up must expire after %s", ttl)
}

// waitingOnFlight counts the goroutines parked in Resolver.shared's select,
// which is where a caller sits once it has joined a flight. Nothing else
// exposes that.
func waitingOnFlight(sharedFrame string) int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, sharedFrame+"(") &&
			strings.Contains(g, " [select]:") {
			n++
		}
	}
	return n
}

// Callers that overlap on one host share one Django call.
func TestEnsureSecretsCollapsesConcurrentTopUps(t *testing.T) {
	f := newFakeDjango(t)
	_, podB, b := twoPods(t, f, newFakeRedis(t))

	// Django answers once released. The release also runs at cleanup,
	// which comes before the server closes, so a failing test never
	// leaves a handler parked.
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	f.mu.Lock()
	f.hold = hold
	f.mu.Unlock()
	before := f.calls.Load()

	// The frame is named from the method itself, so a rename cannot
	// leave the wait looking for text that is gone.
	sharedFrame := strings.TrimSuffix(runtime.FuncForPC(
		reflect.ValueOf(podB.shared).Pointer()).Name(), "-fm")

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
	// late one cannot start a second. A second call means the flight
	// is not shared: stop waiting and let the assertion below say so.
	assert.Eventually(t, func() bool {
		return waitingOnFlight(sharedFrame) == callers ||
			f.calls.Load()-before > 1
	}, 5*time.Second, time.Millisecond)
	release()
	done.Wait()

	assert.Equal(t, int32(1), f.calls.Load()-before,
		"overlapping top-ups must share one Django call")
}

// assertNoCredentialInRedis fails when any value the shared Redis was given
// holds one of the test credentials or one of more.
func assertNoCredentialInRedis(t *testing.T, rd *fakeRedis, more ...string) {
	t.Helper()
	creds := append([]string{testChatKey, testACPBearer}, more...)
	for _, v := range rd.values() {
		for _, c := range creds {
			assert.NotContains(t, v, c, "a credential reached Redis")
		}
	}
}

// ageCredentials moves the time this pod fetched secretsHost's credentials
// from Django back by age, in every place the pod holds them.
func ageCredentials(t *testing.T, r *Resolver, age time.Duration) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.mem[secretsHost]
	require.True(t, ok, "no memory entry for %s to age", secretsHost)
	older := func(tn *Tenant) *Tenant {
		if tn == nil || !tn.SecretsLoaded {
			return tn
		}
		aged := *tn
		aged.ResolvedAt = aged.ResolvedAt.Add(-age)
		return &aged
	}
	e.tenant = older(e.tenant)
	e.held = older(e.held)
	r.mem[secretsHost] = e
}

const unknownStore = `{"error":"unknown store"}`

// The store was switched off after the caches were filled, and Django
// answers 404 for it.
//   - The pod that resolved the store itself holds a live entry with the
//     credentials and serves it until the entry's TTL ends, with no Django
//     call, as it does on every route: the TTL is how long any change to a
//     store takes to reach a pod.
//   - A pod whose entry came from Redis asks Django for the credentials,
//     hears the store is gone and caches that, as the resolve does: 404 on
//     every route, without asking Django again, and a pod reading Redis
//     next hears it from there.
//   - A pod with no cache of the store hears it at the resolve: 404.
func TestADeactivatedStoreIsUnknownOnTheCredentialRoutes(t *testing.T) {
	f := newFakeDjango(t)
	rd := newFakeRedis(t)
	podA, podB, _ := twoPods(t, f, rd)
	f.deactivate()
	before := f.calls.Load()

	rec, seen := serve(MiddlewareWithSecrets(podA, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.Equal(t, testChatKey, seen.ChatAPIKey)
	assert.Equal(t, before, f.calls.Load())

	for _, mw := range []func(http.Handler) http.Handler{
		MiddlewareWithSecrets(podB, testLogger()),
		MiddlewareWithSecrets(podB, testLogger()),
		Middleware(podB, testLogger()),
	} {
		rec, seen := serve(mw)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.JSONEq(t, unknownStore, rec.Body.String())
		assert.Nil(t, seen)
	}
	assert.Equal(t, before+1, f.calls.Load(),
		"Django's 404 is cached: it is asked once")

	podC := newPod(t, f, rd.client())
	_, err := podC.Resolve(t.Context(), secretsHost)
	require.ErrorIs(t, err, ErrUnknownTenant)
	assert.Equal(t, before+1, f.calls.Load(), "pod C must read Redis")

	cold := newPod(t, f, newFakeRedis(t).client())
	for _, mw := range []func(http.Handler) http.Handler{
		Middleware(cold, testLogger()),
		MiddlewareWithSecrets(cold, testLogger()),
	} {
		rec, seen := serve(mw)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Nil(t, seen)
	}
	assertNoCredentialInRedis(t, rd)
}

// Credentials a pod held before Django said the store was gone are never
// served again, even stale, even once the store is back.
func TestAGoneStoreDropsTheHeldCredentials(t *testing.T) {
	f := newFakeDjango(t)
	rd := newFakeRedis(t)
	pod := newPod(t, f, rd.client())
	_, err := pod.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	f.deactivate()
	expireMemory(t, pod, secretsHost)
	rec, _ := serve(MiddlewareWithSecrets(pod, testLogger()))
	require.Equal(t, http.StatusNotFound, rec.Code)

	// The store comes back, another pod writes its config to Redis, and
	// Django goes down before this pod asks it again.
	f.reactivate()
	cfg, _, err := f.client(t).ResolveTenant(t.Context(), secretsHost)
	require.NoError(t, err)
	newPod(t, f, rd.client()).storeRedis(t.Context(), secretsHost, cfg)
	f.goDown()
	expireMemory(t, pod, secretsHost)

	rec, seen := serve(MiddlewareWithSecrets(pod, testLogger()))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Nil(t, seen)
	assertNoCredentialInRedis(t, rd)
}

// "Django says this store does not exist" is one code path: the top-up
// caches it exactly as the resolve does, in memory and in Redis.
func TestTheTopUpCachesAGoneStoreAsTheResolveDoes(t *testing.T) {
	f := newFakeDjango(t)
	viaTopUp := newFakeRedis(t)
	_, podB, b := twoPods(t, f, viaTopUp)
	f.deactivate()
	_, err := podB.EnsureSecrets(t.Context(), b)
	require.ErrorIs(t, err, ErrUnknownTenant)

	viaResolve := newFakeRedis(t)
	cold := newPod(t, f, viaResolve.client())
	_, err = cold.Resolve(t.Context(), secretsHost)
	require.ErrorIs(t, err, ErrUnknownTenant)

	for name, pod := range map[string]*Resolver{
		"top-up": podB, "resolve": cold,
	} {
		pod.mu.RLock()
		e := pod.mem[secretsHost]
		pod.mu.RUnlock()
		assert.True(t, e.negative, name)
		assert.Nil(t, e.tenant, name)
		assertMemoryTTL(t, pod, secretsHost, testNegTTL)
	}
	for name, rd := range map[string]*fakeRedis{
		"top-up": viaTopUp, "resolve": viaResolve,
	} {
		val, ttl, ok := rd.set(redisKey(secretsHost))
		require.True(t, ok, name)
		assert.Equal(t, negativeSentinel, val, name)
		assert.Equal(t, testNegTTL, ttl, name)
	}
}

// Django goes down after a pod's memory entry expired while Redis still
// holds the store's public config. The Redis hit must not cost the pod the
// credentials it held: it serves them stale, as a pod without Redis serves
// its expired entry, until the bound, and then no longer.
func TestCredentialsOutliveARedisHitThroughAnOutage(t *testing.T) {
	f := newFakeDjango(t)
	rd := newFakeRedis(t)
	withRedis := newPod(t, f, rd.client())
	noRedis := newPod(t, f, unreachableRedis())
	pods := map[string]*Resolver{"with Redis": withRedis, "no Redis": noRedis}
	for _, r := range pods {
		got, err := r.Resolve(t.Context(), secretsHost)
		require.NoError(t, err)
		require.True(t, got.SecretsLoaded)
		expireMemory(t, r, secretsHost)
	}
	require.Equal(t, int32(2), f.calls.Load())

	// The Redis hit spends no Django call and carries no credential.
	got, err := withRedis.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	assert.False(t, got.SecretsLoaded)
	assert.Empty(t, got.ChatAPIKey)
	assert.Equal(t, int32(2), f.calls.Load())

	f.goDown()
	for name, r := range pods {
		rec, seen := serve(MiddlewareWithSecrets(r, testLogger()))
		assert.Equal(t, http.StatusNoContent, rec.Code, name)
		require.NotNil(t, seen, name)
		assert.True(t, seen.Stale, name)
		assert.True(t, seen.SecretsLoaded, name)
		assert.Equal(t, testChatKey, seen.ChatAPIKey, name)
		assert.Equal(t, testACPBearer, seen.ACPBearerToken, name)
		// Remembered for the negative TTL, as a stale config is, so a
		// hanging Django is not asked by every request.
		assertTopUpTTL(t, r, testNegTTL)
		asked := f.calls.Load()
		rec, seen = serve(MiddlewareWithSecrets(r, testLogger()))
		assert.Equal(t, http.StatusNoContent, rec.Code, name)
		require.NotNil(t, seen, name)
		assert.Equal(t, testACPBearer, seen.ACPBearerToken, name)
		assert.Equal(t, asked, f.calls.Load(), name)
	}

	// The bound is one TTL after the credentials went stale, two after
	// the pod got them: half a TTL before it they are still served.
	for name, r := range pods {
		ageCredentials(t, r, testTTL+testTTL/2)
		expireMemory(t, r, secretsHost)
		rec, seen := serve(MiddlewareWithSecrets(r, testLogger()))
		assert.Equal(t, http.StatusNoContent, rec.Code, name)
		require.NotNil(t, seen, name)
		assert.Equal(t, testACPBearer, seen.ACPBearerToken, name)
	}

	// Past the bound the credentials are not served: the credential
	// routes answer 503, the others keep serving the config.
	for name, r := range pods {
		ageCredentials(t, r, testTTL)
		expireMemory(t, r, secretsHost)
		rec, seen := serve(MiddlewareWithSecrets(r, testLogger()))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, name)
		assert.Nil(t, seen, name)

		rec, seen = serve(Middleware(r, testLogger()))
		assert.Equal(t, http.StatusNoContent, rec.Code, name)
		require.NotNil(t, seen, name)
		assert.Equal(t, "demostore", seen.SchemaName, name)
		assert.False(t, seen.SecretsLoaded, name)
		assert.Empty(t, seen.ChatAPIKey, name)
		assert.Empty(t, seen.ACPBearerToken, name)
	}
	assertNoCredentialInRedis(t, rd)
}

// A stale answer with credentials is cached for the negative TTL, but never
// past the credentials' bound.
func TestStaleCredentialsAreCachedNoLongerThanTheirBound(t *testing.T) {
	f := newFakeDjango(t)
	longNegTTL := 10 * testTTL
	pod := NewResolver(f.client(t), newFakeRedis(t).client(), testTTL,
		longNegTTL, testLogger(), nil)
	_, err := pod.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	ageCredentials(t, pod, testTTL+testTTL/4)
	expireMemory(t, pod, secretsHost)
	f.goDown()

	rec, seen := serve(MiddlewareWithSecrets(pod, testLogger()))
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.True(t, seen.Stale)
	assertTopUpTTL(t, pod, testTTL-testTTL/4)
}

// A credential rotated in Django replaces the one a pod holds as soon as
// Django is asked: at the first top-up after the entry expired when Django
// is up, at the first one after the stale answer's negative TTL when it was
// down.
func TestARotatedCredentialReplacesTheHeldOne(t *testing.T) {
	const newChat, newACP = "sk-rotated-do-not-cache", "acp-rotated-do-not-cache"
	assertRotated := func(t *testing.T, r *Resolver) {
		t.Helper()
		rec, seen := serve(MiddlewareWithSecrets(r, testLogger()))
		assert.Equal(t, http.StatusNoContent, rec.Code)
		require.NotNil(t, seen)
		assert.False(t, seen.Stale)
		assert.Equal(t, newChat, seen.ChatAPIKey)
		assert.Equal(t, newACP, seen.ACPBearerToken)
	}

	t.Run("Django up", func(t *testing.T) {
		f := newFakeDjango(t)
		rd := newFakeRedis(t)
		pod := newPod(t, f, rd.client())
		_, err := pod.Resolve(t.Context(), secretsHost)
		require.NoError(t, err)
		f.rotate(newChat, newACP)
		expireMemory(t, pod, secretsHost)

		assertRotated(t, pod)
		assertNoCredentialInRedis(t, rd, newChat, newACP)
	})

	t.Run("after an outage", func(t *testing.T) {
		f := newFakeDjango(t)
		rd := newFakeRedis(t)
		pod := newPod(t, f, rd.client())
		_, err := pod.Resolve(t.Context(), secretsHost)
		require.NoError(t, err)
		expireMemory(t, pod, secretsHost)
		f.goDown()
		rec, seen := serve(MiddlewareWithSecrets(pod, testLogger()))
		require.Equal(t, http.StatusNoContent, rec.Code)
		require.NotNil(t, seen)
		assert.Equal(t, testACPBearer, seen.ACPBearerToken)

		f.rotate(newChat, newACP)
		f.comeUp()
		expireMemory(t, pod, secretsHost)
		assertRotated(t, pod)
		assertNoCredentialInRedis(t, rd, newChat, newACP)
	})
}

// A resolve that read Redis while a top-up asked Django must not replace
// the tenant the top-up stored with the public config it read.
func TestARedisReadDoesNotOverwriteATopUp(t *testing.T) {
	f := newFakeDjango(t)
	rd := newFakeRedis(t)
	_, podB, b := twoPods(t, f, rd)
	expireMemory(t, podB, secretsHost)

	entered, release := rd.parkNextGet()
	type result struct {
		t   *Tenant
		err error
	}
	resolved := make(chan result, 1)
	go func() {
		got, err := podB.Resolve(context.Background(), secretsHost)
		resolved <- result{got, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the resolve never read Redis")
	}
	full, err := podB.EnsureSecrets(t.Context(), b)
	require.NoError(t, err)
	release()
	res := <-resolved
	require.NoError(t, res.err)
	assert.Same(t, full, res.t, "the resolve must answer the topped-up tenant")

	before := f.calls.Load()
	again, err := podB.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	assert.Same(t, full, again)
	assert.Equal(t, before, f.calls.Load())
	assertNoCredentialInRedis(t, rd)
}

// A caller that resolved before another request filled the memory tier
// still holds the old tenant; its top-up reads memory before asking Django.
func TestEnsureSecretsReadsMemoryInsideItsFlight(t *testing.T) {
	f := newFakeDjango(t)
	_, podB, b := twoPods(t, f, newFakeRedis(t))

	full, err := podB.EnsureSecrets(t.Context(), b)
	require.NoError(t, err)
	before := f.calls.Load()
	again, err := podB.EnsureSecrets(t.Context(), b)
	require.NoError(t, err)
	assert.Same(t, full, again)
	assert.Equal(t, before, f.calls.Load())

	// And a store found gone meanwhile is gone for it too.
	gone := newFakeDjango(t)
	_, podC, c := twoPods(t, gone, newFakeRedis(t))
	gone.deactivate()
	_, err = podC.EnsureSecrets(t.Context(), c)
	require.ErrorIs(t, err, ErrUnknownTenant)
	before = gone.calls.Load()
	_, err = podC.EnsureSecrets(t.Context(), c)
	require.ErrorIs(t, err, ErrUnknownTenant)
	assert.Equal(t, before, gone.calls.Load())
}

// unrecognised is the cause the resolver logs when Django answers a call
// carrying the internal token as it answers any caller.
const unrecognised = "Django did not recognise the gateway's internal token"

// A rotation of the internal secret that reached Django first: Django
// answers the gateway as it answers anyone, 200 with the public body. That
// is a valid public config, not a store without credentials.
func TestAnUnrecognisedTokenIsNotAStoreWithoutCredentials(t *testing.T) {
	f := newFakeDjango(t)
	f.rotateToken("rotated-on-django-only")
	rd := newFakeRedis(t)
	var logs capturedLog
	pod := newPodLogging(t, f, rd.client(), logs.logger())

	got, err := pod.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	assert.False(t, got.SecretsLoaded)
	assert.Equal(t, "demostore", got.SchemaName)
	assert.Equal(t, 1, strings.Count(logs.String(), `msg="`+unrecognised))
	assert.Contains(t, logs.String(), `level=ERROR msg="`+unrecognised)
	assert.Contains(t, logs.String(), "tenant=demostore")
	assertMemoryTTL(t, pod, secretsHost, testTTL)
	pod.mu.RLock()
	assert.Nil(t, pod.mem[secretsHost].held, "nothing is held")
	pod.mu.RUnlock()
	// The public config refreshes the shared entry.
	raw, ok := rd.raw(redisKey(secretsHost))
	require.True(t, ok)
	assert.Contains(t, raw, `"schemaName":"demostore"`)

	rec, seen := serve(Middleware(pod, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.False(t, seen.SecretsLoaded)

	// The credential routes: a failed top-up, remembered for the negative
	// TTL: one Django call and one error line, the resolver's with its
	// cause; the middleware does not log it again.
	before := f.calls.Load()
	for range 3 {
		rec, seen := serve(MiddlewareWithSecrets(pod, logs.logger()))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Nil(t, seen)
	}
	assert.Equal(t, before+1, f.calls.Load())
	assert.Equal(t, 2, strings.Count(logs.String(), `msg="`+unrecognised))
	assert.NotContains(t, logs.String(), "tenant secret refresh failed")
	assertTopUpTTL(t, pod, testNegTTL)

	// After it, Django is asked again.
	expireMemory(t, pod, secretsHost)
	rec, _ = serve(MiddlewareWithSecrets(pod, logs.logger()))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, before+2, f.calls.Load())
	assertNoCredentialInRedis(t, rd)
}

// The same after a Redis hit: the top-up is refused, not answered with
// empty credentials.
func TestAnUnrecognisedTopUpFailsTheCredentialRoutes(t *testing.T) {
	f := newFakeDjango(t)
	rd := newFakeRedis(t)
	_, podB, b := twoPods(t, f, rd)
	f.rotateToken("rotated-on-django-only")

	_, err := podB.EnsureSecrets(t.Context(), b)
	require.ErrorContains(t, err, unrecognised)
	rec, seen := serve(MiddlewareWithSecrets(podB, testLogger()))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Nil(t, seen)
	rec, _ = serve(Middleware(podB, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assertNoCredentialInRedis(t, rd)
}

// While Django does not recognise the token, a pod serves the credentials
// it held within their bound; once Django recognises it again the first
// call after the remembered answer's negative TTL loads them, without a
// restart.
func TestATokenRotationIsBridgedByHeldCredentials(t *testing.T) {
	f := newFakeDjango(t)
	rd := newFakeRedis(t)
	var logs capturedLog
	pod := newPodLogging(t, f, rd.client(), logs.logger())
	_, err := pod.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)
	expireMemory(t, pod, secretsHost)
	f.rotateToken("rotated-on-django-only")

	rec, seen := serve(MiddlewareWithSecrets(pod, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.True(t, seen.Stale)
	assert.Equal(t, testACPBearer, seen.ACPBearerToken)
	assert.Equal(t, 1, strings.Count(logs.String(), `msg="`+unrecognised))

	// The held answer does not replace the config Django just sent: the
	// other routes serve it, fresh.
	rec, seen = serve(Middleware(pod, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.False(t, seen.Stale)
	assert.False(t, seen.SecretsLoaded)
	assertMemoryTTL(t, pod, secretsHost, testTTL)

	// Past the bound it is refused.
	ageCredentials(t, pod, 2*testTTL)
	expireMemory(t, pod, secretsHost)
	rec, seen = serve(MiddlewareWithSecrets(pod, testLogger()))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Nil(t, seen)

	// The gateway gets the new secret Django holds.
	f.rotateToken(testInternalSecret)
	expireMemory(t, pod, secretsHost)
	rec, seen = serve(MiddlewareWithSecrets(pod, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.False(t, seen.Stale)
	assert.True(t, seen.SecretsLoaded)
	assert.Equal(t, testChatKey, seen.ChatAPIKey)
	assertNoCredentialInRedis(t, rd)
}

// A store that has no chat key and no ACP token: Django sends both keys,
// empty. It is loaded with empty credentials and nothing is logged.
func TestAStoreWithoutCredentialsIsLoaded(t *testing.T) {
	f := newFakeDjango(t)
	f.rotate("", "")
	var logs capturedLog
	pod := newPodLogging(t, f, newFakeRedis(t).client(), logs.logger())

	rec, seen := serve(MiddlewareWithSecrets(pod, testLogger()))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.True(t, seen.SecretsLoaded)
	assert.False(t, seen.Stale)
	assert.Empty(t, seen.ChatAPIKey)
	assert.Empty(t, seen.ACPBearerToken)
	assert.Equal(t, int32(1), f.calls.Load())
	assert.Empty(t, logs.String())
}

// Held credentials belong to a store, not a host. The host moved to another
// store: the credential routes never serve the old store's credentials or
// config on it, and the other routes serve the new store.
func TestHeldCredentialsDoNotFollowAHostToAnotherStore(t *testing.T) {
	assertMoved := func(t *testing.T, pod *Resolver) {
		t.Helper()
		rec, seen := serve(MiddlewareWithSecrets(pod, testLogger()))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Nil(t, seen)
		rec, seen = serve(Middleware(pod, testLogger()))
		assert.Equal(t, http.StatusNoContent, rec.Code)
		require.NotNil(t, seen)
		assert.Equal(t, "acme", seen.SchemaName)
		assert.Empty(t, seen.ChatAPIKey)
		assert.Empty(t, seen.ACPBearerToken)
		pod.mu.RLock()
		assert.Nil(t, pod.mem[secretsHost].held, "nothing is held")
		pod.mu.RUnlock()
	}

	// Another pod wrote the new store's config to Redis; Django is down.
	t.Run("Redis names another store", func(t *testing.T) {
		f := newFakeDjango(t)
		rd := newFakeRedis(t)
		pod := newPod(t, f, rd.client())
		_, err := pod.Resolve(t.Context(), secretsHost)
		require.NoError(t, err)
		f.moveTo("acme")
		f.rotate("sk-acme-do-not-cache", "acp-acme-do-not-cache")
		cfg, _, err := f.client(t).ResolveTenant(t.Context(), secretsHost)
		require.NoError(t, err)
		newPod(t, f, rd.client()).storeRedis(t.Context(), secretsHost, cfg)
		f.goDown()
		expireMemory(t, pod, secretsHost)

		assertMoved(t, pod)
		assertNoCredentialInRedis(t, rd,
			"sk-acme-do-not-cache", "acp-acme-do-not-cache")
	})

	// Django names the new store, without recognising the token.
	t.Run("Django names another store", func(t *testing.T) {
		f := newFakeDjango(t)
		rd := newFakeRedis(t)
		pod := newPod(t, f, rd.client())
		_, err := pod.Resolve(t.Context(), secretsHost)
		require.NoError(t, err)
		f.moveTo("acme")
		f.rotateToken("rotated-on-django-only")
		expireMemory(t, pod, secretsHost)

		assertMoved(t, pod)
		raw, ok := rd.raw(redisKey(secretsHost))
		require.True(t, ok)
		assert.Contains(t, raw, `"schemaName":"acme"`)
		assertNoCredentialInRedis(t, rd)
	})
}

// An entry another flight made live while this one waited on Django is
// newer than any stale answer: neither the stale config nor the stale
// credentials replace it.
func TestAStaleAnswerNeverReplacesALiveEntry(t *testing.T) {
	f := newFakeDjango(t)
	pod := newPod(t, f, newFakeRedis(t).client())
	full, err := pod.Resolve(t.Context(), secretsHost)
	require.NoError(t, err)

	got, err := pod.serveStale(t.Context(), secretsHost, django.ErrUpstreamDown)
	require.NoError(t, err)
	assert.Same(t, full, got)
	got, err = pod.answerSecrets(t.Context(), secretsHost,
		django.ErrUpstreamDown)
	require.NoError(t, err)
	assert.Same(t, full, got)
	pod.mu.RLock()
	e := pod.mem[secretsHost]
	pod.mu.RUnlock()
	assert.Same(t, full, e.tenant)
	assert.False(t, e.tenant.Stale)
	assert.Zero(t, e.topUp, "nothing is remembered over a live answer")

	pod.storeMemory(secretsHost, memEntry{
		negative: true, expires: time.Now().Add(testNegTTL),
	})
	_, err = pod.serveStale(t.Context(), secretsHost, django.ErrUpstreamDown)
	require.ErrorIs(t, err, ErrUnknownTenant)
	_, err = pod.answerSecrets(t.Context(), secretsHost,
		django.ErrUpstreamDown)
	require.ErrorIs(t, err, ErrUnknownTenant)
}

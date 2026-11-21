package identity

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

type profile struct{ store, token string }

// countingProbe answers every token at schema "acme" as valid and at any
// other store as Django's 401, counting the probes.
func countingProbe(calls *atomic.Int32) Probe[*profile] {
	return func(
		_ context.Context, t *tenant.Tenant, token string,
	) (*profile, error) {
		calls.Add(1)
		if t.SchemaName != "acme" {
			return nil, django.ErrUnauthorized
		}
		return &profile{store: t.SchemaName, token: token}, nil
	}
}

func store(schema string) *tenant.Tenant {
	return &tenant.Tenant{SchemaName: schema, Domain: schema + ".test"}
}

// The same token at two stores is two entries and two verdicts: valid
// where the store says so, invalid where it does not, each cached for
// its own store.
func TestVerifierKeepsEachStoresVerdictApart(t *testing.T) {
	var calls atomic.Int32
	v := NewVerifier(countingProbe(&calls), testLogger())
	ctx := t.Context()

	for range 2 {
		got, err := v.Verify(ctx, store("acme"), "token")
		require.NoError(t, err)
		assert.Equal(t, &profile{store: "acme", token: "token"}, got)
		_, err = v.Verify(ctx, store("alpha"), "token")
		require.ErrorIs(t, err, ErrInvalidToken)
	}
	assert.EqualValues(t, 2, calls.Load(), "one probe per store")
	assert.Len(t, v.cache, 2)
}

// A verdict, valid or invalid, answers for 60 seconds and no longer.
func TestVerifierVerdictExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		v := NewVerifier(countingProbe(&calls), testLogger())
		verify := func() {
			_, err := v.Verify(t.Context(), store("acme"), "token")
			require.NoError(t, err)
			_, err = v.Verify(t.Context(), store("alpha"), "token")
			require.ErrorIs(t, err, ErrInvalidToken)
		}

		verify()
		time.Sleep(59 * time.Second)
		verify()
		assert.EqualValues(t, 2, calls.Load(), "cached within the TTL")

		time.Sleep(time.Second)
		verify()
		assert.EqualValues(t, 4, calls.Load(), "asked again once expired")
	})
}

// The cache never holds more than cacheMax verdicts. A full cache drops
// the verdicts that expired, else the one least recently used, never
// every verdict: a bearer in use keeps its own however many others are
// probed, and is not asked again.
func TestVerifierIsBoundedAndKeepsTheBearersInUse(t *testing.T) {
	var calls atomic.Int32
	v := NewVerifier(countingProbe(&calls), testLogger())
	verify := func(token string) {
		t.Helper()
		_, err := v.Verify(t.Context(), store("acme"), token)
		require.NoError(t, err)
		require.LessOrEqual(t, len(v.cache), cacheMax)
	}
	verify("in use")
	for i := range cacheMax + 1 {
		verify(strconv.Itoa(i))
		verify("in use")
	}
	assert.Len(t, v.cache, cacheMax)
	assert.EqualValues(t, cacheMax+2, calls.Load(),
		"the bearer in use was probed once")
	// The two verdicts past the cap pushed out the two least recently
	// used, "0" and "1", and no other: an arbitrary victim would not be
	// those two.
	acme := store("acme")
	v.mu.Lock()
	assert.NotContains(t, v.cache, cacheKey(acme, "0"))
	assert.NotContains(t, v.cache, cacheKey(acme, "1"))
	for _, kept := range []string{"2", strconv.Itoa(cacheMax), "in use"} {
		assert.Contains(t, v.cache, cacheKey(acme, kept), kept)
	}
	v.mu.Unlock()

	// With some verdicts expired, exactly those go: every other one,
	// the bearer in use's among them, stays.
	inUse := cacheKey(acme, "in use")
	v.mu.Lock()
	expired := map[string]bool{}
	var kept []string
	for k, e := range v.cache {
		if len(expired) < 10 && k != inUse {
			e.expires = time.Now().Add(-time.Second)
			v.cache[k] = e
			expired[k] = true
			continue
		}
		kept = append(kept, k)
	}
	v.mu.Unlock()
	require.Len(t, expired, 10)
	before := calls.Load()
	verify("new")
	assert.Equal(t, before+1, calls.Load())
	v.mu.Lock()
	for k := range expired {
		assert.NotContains(t, v.cache, k, "expired")
	}
	for _, k := range kept {
		assert.Contains(t, v.cache, k, "not expired")
	}
	assert.Contains(t, v.cache, cacheKey(acme, "new"))
	assert.Len(t, v.cache, cacheMax-len(expired)+1)
	v.mu.Unlock()
	verify("in use")
	assert.Equal(t, before+1, calls.Load(), "the bearer in use kept its own")
}

// heldProbe holds every probe until release is closed, counting them and
// recording whether the context it ran on had ended.
type heldProbe struct {
	calls   atomic.Int32
	release chan struct{}
	ended   atomic.Bool
}

func (h *heldProbe) probe(
	ctx context.Context, t *tenant.Tenant, token string,
) (*profile, error) {
	h.calls.Add(1)
	<-h.release
	h.ended.Store(ctx.Err() != nil)
	return &profile{store: t.SchemaName, token: token}, nil
}

// Callers that ask about one uncached token at once share one probe.
func TestVerifierConcurrentCallersShareOneProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &heldProbe{release: make(chan struct{})}
		v := NewVerifier(h.probe, testLogger())

		const callers = 8
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				_, errs[i] = v.Verify(t.Context(), store("acme"), "token")
			})
		}
		// Every caller is now waiting: on the probe, or on its result.
		synctest.Wait()
		assert.EqualValues(t, 1, h.calls.Load())

		close(h.release)
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		assert.EqualValues(t, 1, h.calls.Load())
	})
}

// The caller whose call started the probe hanging up fails nobody else:
// the probe runs on, and the callers still waiting get its verdict.
func TestVerifierCallerHangingUpFailsNoOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &heldProbe{release: make(chan struct{})}
		v := NewVerifier(h.probe, testLogger())

		first, hangUp := context.WithCancel(t.Context())
		var firstErr, otherErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			_, firstErr = v.Verify(first, store("acme"), "token")
		})
		synctest.Wait()
		wg.Go(func() {
			_, otherErr = v.Verify(t.Context(), store("acme"), "token")
		})
		synctest.Wait()

		hangUp()
		synctest.Wait()
		close(h.release)
		wg.Wait()

		require.ErrorIs(t, firstErr, context.Canceled)
		require.NoError(t, otherErr)
		assert.False(t, h.ended.Load(), "the probe ran on")
		assert.EqualValues(t, 1, h.calls.Load())
	})
}

// A caller that has already hung up starts no probe: it gets its own
// context's error, Django is not asked, and nothing is cached. The bubble
// lets every goroutine the call started finish before the count is read.
func TestVerifierCallerGoneBeforehandStartsNoProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		v := NewVerifier(countingProbe(&calls), testLogger())
		ctx, hangUp := context.WithCancel(t.Context())
		hangUp()

		_, err := v.Verify(ctx, store("acme"), "token")
		synctest.Wait()

		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, calls.Load())
		assert.Empty(t, v.cache)
	})
}

// pairs are distinct (store, token) pairs: one token at two stores, two
// tokens at one store, and two pairs whose store and token run together
// into the same text.
var pairs = [][2]string{
	{"acme", "t1"}, {"acme", "t2"}, {"alpha", "t1"}, {"ab", "c"}, {"a", "bc"},
}

// Concurrent callers for distinct (store, token) pairs share no probe:
// each pair is probed once, and each caller gets its own pair's verdict.
func TestVerifierConcurrentPairsGetTheirOwnVerdicts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &heldProbe{release: make(chan struct{})}
		v := NewVerifier(h.probe, testLogger())

		got := make([]*profile, len(pairs))
		var wg sync.WaitGroup
		for i, p := range pairs {
			wg.Go(func() {
				got[i], _ = v.Verify(t.Context(), store(p[0]), p[1])
			})
		}
		synctest.Wait()
		assert.EqualValues(t, len(pairs), h.calls.Load())
		close(h.release)
		wg.Wait()

		for i, p := range pairs {
			assert.Equal(t, &profile{store: p[0], token: p[1]}, got[i])
		}
	})
}

// A store and a token that run together into one text are still two
// entries: ("ab", "c") is not ("a", "bc").
func TestVerifierKeepsStoreAndTokenApart(t *testing.T) {
	var calls atomic.Int32
	v := NewVerifier(func(
		_ context.Context, t *tenant.Tenant, token string,
	) (*profile, error) {
		calls.Add(1)
		return &profile{store: t.SchemaName, token: token}, nil
	}, testLogger())

	for range 2 {
		for _, p := range [][2]string{{"ab", "c"}, {"a", "bc"}} {
			got, err := v.Verify(t.Context(), store(p[0]), p[1])
			require.NoError(t, err)
			assert.Equal(t, &profile{store: p[0], token: p[1]}, got)
		}
	}
	assert.EqualValues(t, 2, calls.Load())
	assert.Len(t, v.cache, 2)
}

// A panic in the probe comes back as an error, without the panic text,
// instead of ending the process from singleflight's own goroutine.
func TestVerifierReturnsAProbePanicAsError(t *testing.T) {
	v := NewVerifier(func(
		context.Context, *tenant.Tenant, string,
	) (*profile, error) {
		panic("boom")
	}, testLogger())

	_, err := v.Verify(t.Context(), store("acme"), "token")

	require.EqualError(t, err, "identity: verify: panicked")
	assert.Empty(t, v.cache)
}

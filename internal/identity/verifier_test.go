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

type profile struct{ store string }

// countingProbe answers every token at schema "acme" as valid and at any
// other store as Django's 401, counting the probes.
func countingProbe(calls *atomic.Int32) Probe[*profile] {
	return func(
		_ context.Context, t *tenant.Tenant, _ string,
	) (*profile, error) {
		calls.Add(1)
		if t.SchemaName != "acme" {
			return nil, django.ErrUnauthorized
		}
		return &profile{store: t.SchemaName}, nil
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
		assert.Equal(t, &profile{store: "acme"}, got)
		_, err = v.Verify(ctx, store("alpha"), "token")
		require.ErrorIs(t, err, ErrInvalidToken)
	}
	assert.EqualValues(t, 2, calls.Load(), "one probe per store")
	assert.Len(t, v.cache, 2)
}

// A verdict, valid or invalid, answers for cacheTTL and no longer.
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
		time.Sleep(cacheTTL - time.Second)
		verify()
		assert.EqualValues(t, 2, calls.Load(), "cached within the TTL")

		time.Sleep(time.Second)
		verify()
		assert.EqualValues(t, 4, calls.Load(), "asked again once expired")
	})
}

// The cache never holds more than cacheMax verdicts.
func TestVerifierIsBounded(t *testing.T) {
	var calls atomic.Int32
	v := NewVerifier(countingProbe(&calls), testLogger())
	for i := range cacheMax + 1 {
		_, err := v.Verify(t.Context(), store("acme"), strconv.Itoa(i))
		require.NoError(t, err)
		require.LessOrEqual(t, len(v.cache), cacheMax)
	}
}

// heldProbe holds every probe until release is closed, counting them and
// recording whether the context it ran on had ended.
type heldProbe struct {
	calls   atomic.Int32
	release chan struct{}
	ended   atomic.Bool
}

func (h *heldProbe) probe(
	ctx context.Context, t *tenant.Tenant, _ string,
) (*profile, error) {
	h.calls.Add(1)
	<-h.release
	h.ended.Store(ctx.Err() != nil)
	return &profile{store: t.SchemaName}, nil
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
			assert.NoError(t, err)
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

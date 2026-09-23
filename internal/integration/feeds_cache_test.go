//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/feeds"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// gatedDjango wraps the feeds fake so the next catalog sweep blocks on
// its first product page until release is called — the window in which
// an invalidation or a client disconnect has to be handled.
type gatedDjango struct {
	next     http.Handler
	sweeps   atomic.Int32
	armed    atomic.Bool
	entered  chan struct{}
	released chan struct{}
	once     sync.Once
}

func newGatedDjango(t *testing.T) *gatedDjango {
	g := &gatedDjango{
		next:     feedsFakeDjango(t),
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
	g.armed.Store(true)
	return g
}

func (g *gatedDjango) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/product" && r.URL.Query().Get("page") == "1" {
		g.sweeps.Add(1)
		if g.armed.CompareAndSwap(true, false) {
			close(g.entered)
			<-g.released
		}
	}
	g.next.ServeHTTP(w, r)
}

func (g *gatedDjango) release() { g.once.Do(func() { close(g.released) }) }

func newFeedService(t *testing.T, djangoURL string) *feeds.Service {
	t.Helper()
	log := quietLogger()
	dj := django.New(djangoURL+"/api/v1", "api.example.test", "test-secret",
		5*time.Second, log, obs.NewMetrics())
	return feeds.NewService(dj, startRedis(t), log,
		"https://{assets_host}/x/{path}", "assets.platform.test",
		time.Hour, 24*time.Hour)
}

func feedTenant() *tenant.Tenant {
	return &tenant.Tenant{
		SchemaName:      "demostore",
		DefaultLocale:   "el",
		DefaultCurrency: "EUR",
		Name:            "Demo Store",
		Domain:          "shop.example.test",
	}
}

// A sweep that read the catalog before an invalidation must not land as
// fresh afterwards: it is served (the newest complete feed there is) but
// stale, so the next request regenerates with the new prices.
func TestFeedInvalidationDuringSweepIsNotLostToIt(t *testing.T) {
	g := newGatedDjango(t)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	svc := newFeedService(t, srv.URL)
	ten := feedTenant()
	ctx := context.Background()

	done := make(chan feeds.Meta, 1)
	go func() {
		_, meta, err := svc.Get(ctx, ten, feeds.KindGoogle)
		assert.NoError(t, err)
		done <- meta
	}()
	<-g.entered
	_, err := svc.Invalidate(ctx, ten.SchemaName)
	require.NoError(t, err)
	g.release()

	overtaken := <-done
	assert.True(t, overtaken.GeneratedAt.IsZero(),
		"a sweep overtaken by an invalidation must be stored stale")

	require.Eventually(t, func() bool {
		_, meta, err := svc.Get(ctx, ten, feeds.KindGoogle)
		return err == nil && !meta.GeneratedAt.IsZero()
	}, 10*time.Second, 50*time.Millisecond,
		"the stale entry must trigger a regeneration")
	assert.EqualValues(t, 2, g.sweeps.Load())
}

// The first caller of a cold feed giving up must not cancel the sweep
// the other callers share: it completes and fills the cache.
func TestFeedSweepOutlivesTheFirstCaller(t *testing.T) {
	g := newGatedDjango(t)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	svc := newFeedService(t, srv.URL)
	ten := feedTenant()

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, _, err := svc.Get(ctx, ten, feeds.KindMeta)
		first <- err
	}()
	<-g.entered
	cancel()
	require.ErrorIs(t, <-first, context.Canceled)
	g.release()

	require.Eventually(t, func() bool {
		_, meta, err := svc.Get(context.Background(), ten, feeds.KindACP)
		return err == nil && !meta.GeneratedAt.IsZero()
	}, 10*time.Second, 50*time.Millisecond)
	assert.EqualValues(t, 1, g.sweeps.Load(),
		"the abandoned sweep filled the cache; no second sweep ran")
}

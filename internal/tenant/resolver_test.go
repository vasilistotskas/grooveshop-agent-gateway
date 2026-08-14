package tenant

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

const websideJSON = `{
	"schemaName": "webside",
	"storeName": "Webside",
	"defaultLocale": "el",
	"defaultCurrency": "EUR",
	"primaryDomain": "shop.example.test",
	"loyaltyEnabled": true,
	"blogEnabled": true
}`

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError + 1,
	}))
}

// unreachableRedis exercises the resolver's Redis-degraded path in unit
// tests; the Redis tier itself is covered by the integration suite.
func unreachableRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:            "127.0.0.1:1",
		DialTimeout:     20 * time.Millisecond,
		ReadTimeout:     20 * time.Millisecond,
		WriteTimeout:    20 * time.Millisecond,
		MaxRetries:      -1,
		PoolSize:        1,
		MinIdleConns:    0,
		ConnMaxIdleTime: time.Millisecond,
	})
}

// fakeDjango serves tenant/resolve for one known domain and counts calls.
type fakeDjango struct {
	calls atomic.Int32
	mu    sync.Mutex
	fail  bool
	srv   *httptest.Server
}

func newFakeDjango(t *testing.T) *fakeDjango {
	t.Helper()
	f := &fakeDjango{}
	f.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			f.calls.Add(1)
			f.mu.Lock()
			failing := f.fail
			f.mu.Unlock()
			if failing {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("domain") == "shop.example.test" {
				_, _ = w.Write([]byte(websideJSON))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail": "Store not found."}`))
		}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDjango) setFail(v bool) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}

func (f *fakeDjango) client(t *testing.T) *django.Client {
	t.Helper()
	return django.New(f.srv.URL+"/api/v1", "api.example.test", "test-secret",
		time.Second, testLogger(), nil)
}

func newTestResolver(t *testing.T, f *fakeDjango, ttl time.Duration) *Resolver {
	t.Helper()
	return NewResolver(f.client(t), unreachableRedis(), ttl, 50*time.Millisecond,
		testLogger(), nil)
}

func TestResolveCachesInMemory(t *testing.T) {
	f := newFakeDjango(t)
	r := newTestResolver(t, f, time.Minute)

	first, err := r.Resolve(context.Background(), "SHOP.example.test:443")
	require.NoError(t, err)
	assert.Equal(t, "webside", first.SchemaName)
	assert.Equal(t, "shop.example.test", first.Domain)
	assert.False(t, first.Stale)

	second, err := r.Resolve(context.Background(), "shop.example.test")
	require.NoError(t, err)
	assert.Equal(t, "webside", second.SchemaName)
	assert.Equal(t, int32(1), f.calls.Load(),
		"second resolve must be served from memory")
}

func TestResolveUnknownDomainNegativeCache(t *testing.T) {
	f := newFakeDjango(t)
	r := newTestResolver(t, f, time.Minute)

	_, err := r.Resolve(context.Background(), "nope.example.test")
	assert.ErrorIs(t, err, ErrUnknownTenant)

	_, err = r.Resolve(context.Background(), "nope.example.test")
	assert.ErrorIs(t, err, ErrUnknownTenant)
	assert.Equal(t, int32(1), f.calls.Load(),
		"negative result must be cached")
}

func TestResolveSingleflightCollapsesConcurrentMisses(t *testing.T) {
	f := newFakeDjango(t)
	r := newTestResolver(t, f, time.Minute)

	var wg sync.WaitGroup
	for range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Resolve(context.Background(), "shop.example.test")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), f.calls.Load(),
		"concurrent misses must collapse into one upstream call")
}

func TestResolveServesStaleWhenDjangoDown(t *testing.T) {
	f := newFakeDjango(t)
	r := newTestResolver(t, f, 10*time.Millisecond)

	_, err := r.Resolve(context.Background(), "shop.example.test")
	require.NoError(t, err)

	time.Sleep(20 * time.Millisecond) // let the memory entry expire
	f.setFail(true)

	got, err := r.Resolve(context.Background(), "shop.example.test")
	require.NoError(t, err)
	assert.True(t, got.Stale)
	assert.Equal(t, "webside", got.SchemaName)
}

func TestResolveErrorsWithoutStaleEntry(t *testing.T) {
	f := newFakeDjango(t)
	f.setFail(true)
	r := newTestResolver(t, f, time.Minute)

	_, err := r.Resolve(context.Background(), "shop.example.test")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnknownTenant)
}

func TestNormalizeHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Shop.Example.Test", "shop.example.test"},
		{"shop.example.test:8443", "shop.example.test"},
		{"  shop.example.test ", "shop.example.test"},
		{"", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, NormalizeHost(tc.in), "input %q", tc.in)
	}
}

package feeds

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
)

// oneProductCatalog serves an empty category tree and a one-page catalog,
// so a generation succeeds.
func oneProductCatalog(t *testing.T) *django.Client {
	t.Helper()
	return storeCatalog(t, 1)
}

// storeCatalog serves an empty category tree and a catalog of pages
// pages, one product each.
func storeCatalog(t *testing.T, pages int) *django.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/product/category/all") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
				return
			}
			writePage(w, pages, r.URL.Query().Get("page"))
		}))
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, discard, obs.NewMetrics())
}

// readBack runs one request through a feed that is not cached: the
// generation succeeds and is written, and the read that follows it gets
// readBackErr, or a miss when that is nil. cancel is the request's.
func readBack(
	t *testing.T, body bool,
	readBackErr func(cancel context.CancelFunc) error,
) (*scriptedRedis, string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var reads atomic.Int32
	r := &scriptedRedis{answer: func(cmd redis.Cmder) error {
		switch c := cmd.(type) {
		case *redis.SliceCmd:
			if c.Name() != "mget" {
				return nil
			}
			if reads.Add(1) > 1 && readBackErr != nil {
				return readBackErr(cancel)
			}
			c.SetVal(make([]any, len(c.Args())-1))
		case *redis.StringCmd:
			return redis.Nil // no epoch yet
		}
		return nil
	}}
	// The generation logs on its own goroutine; Body and Meta return
	// only once it has finished.
	var logs syncBuffer
	s := NewService(oneProductCatalog(t), r.client(),
		slog.New(slog.NewJSONHandler(&logs, nil)), obs.NewMetrics(), "", "",
		time.Hour, 24*time.Hour, "test", storefront.NewSite("el"))

	var err error
	if body {
		_, _, err = s.Body(ctx, demoTenant(), KindGoogle)
	} else {
		_, err = s.Meta(ctx, demoTenant(), KindGoogle)
	}
	require.Equal(t, int32(2), reads.Load(), "the read, then the read-back")
	require.True(t, r.set("ag:demostore:feed:rss"), "generated")
	return r, logs.String(), err
}

const goneLine = msgGone

// A generation whose blob is broken when read back is marked like one
// gone, and logged as what it is.
func TestBodyReadBackOfABrokenBlob(t *testing.T) {
	broken := cachedRSS(t, []byte("not gzip"), len(testFeed))
	var reads atomic.Int32
	r := &scriptedRedis{answer: func(cmd redis.Cmder) error {
		switch c := cmd.(type) {
		case *redis.SliceCmd:
			if c.Name() != "mget" {
				return nil
			}
			vals := make([]any, len(c.Args())-1)
			if reads.Add(1) > 1 {
				vals[0] = broken[dataKey("demostore", formatRSS)]
				vals[1] = broken[metaKey("demostore", formatRSS)]
			}
			c.SetVal(vals)
		case *redis.StringCmd:
			return redis.Nil
		}
		return nil
	}}
	var logs syncBuffer
	s := NewService(oneProductCatalog(t), r.client(),
		slog.New(slog.NewJSONHandler(&logs, nil)), obs.NewMetrics(), "", "",
		time.Hour, 24*time.Hour, "test", storefront.NewSite("el"))

	_, _, err := s.Body(context.Background(), demoTenant(), KindGoogle)
	require.ErrorIs(t, err, errGenerationNotCached)
	assert.True(t, r.set("ag:demostore:feed:failed"))
	assert.Contains(t, logs.String(), msgBroken)
	assert.Contains(t, logs.String(), brokenLine)
	assert.NotContains(t, logs.String(), goneLine)
}

func TestReadBackAfterGeneration(t *testing.T) {
	for _, step := range []struct {
		name string
		body bool
	}{{"Meta", false}, {"Body", true}} {
		// The crawler hung up while it waited: the read-back fails with
		// its context, which is the answer, and the feed is not marked.
		t.Run(step.name+"/caller left", func(t *testing.T) {
			r, logs, err := readBack(t, step.body,
				func(cancel context.CancelFunc) error {
					cancel()
					return context.Canceled
				})
			require.ErrorIs(t, err, context.Canceled)
			assert.False(t, r.set("ag:demostore:feed:failed"))
			assert.NotContains(t, logs, goneLine)
		})
		// Redis failed the read for a moment: the feed was written, so it
		// is not marked failed; the request fails with the read's error.
		t.Run(step.name+"/redis error", func(t *testing.T) {
			readErr := errors.New("connection reset")
			r, logs, err := readBack(t, step.body,
				func(context.CancelFunc) error { return readErr })
			require.ErrorIs(t, err, readErr)
			assert.False(t, r.set("ag:demostore:feed:failed"))
			assert.NotContains(t, logs, goneLine)
		})
		// The read-back really found no entry: marked, and logged.
		t.Run(step.name+"/gone", func(t *testing.T) {
			r, logs, err := readBack(t, step.body, nil)
			require.ErrorIs(t, err, errGenerationNotCached)
			assert.True(t, r.set("ag:demostore:feed:failed"))
			assert.Contains(t, logs, goneLine)
		})
	}
}

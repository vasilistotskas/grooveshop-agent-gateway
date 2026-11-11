//go:build integration

package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/server"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// pagedProductJSON renders one product row for the paged catalog fake.
func pagedProductJSON(id int) string {
	return fmt.Sprintf(`{
		"id": %d,
		"translations": {"el": {"name": "Προϊόν %d", "description": "Περιγραφή %d"}},
		"slug": "product-%d",
		"category": 1,
		"variantGroup": null,
		"brandName": null,
		"effectivePrice": 12.40,
		"priorPrice": null,
		"announcedDiscountPercent": null,
		"stock": 3,
		"active": true,
		"mainImagePath": "media/uploads/products/p%d.jpg",
		"uuid": "00000000-0000-0000-0000-%012d"
	}`, id, id, id, id, id, id)
}

func feedsFakeDjango(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tenant/resolve",
		func(w http.ResponseWriter, _ *http.Request) {
			serveFixture(t, w, "tenant_resolve_demostore.json")
		})
	mux.HandleFunc("GET /api/v1/product/category/all",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":1,"translations":{"el":{"name":"Κατηγορία"}},"slug":"cat","active":true,"parent":null,"level":0,"treeId":1}]`))
		})
	// 3 pages x 2 products, exercising the concurrent paged sweep.
	mux.HandleFunc("GET /api/v1/product/feed",
		func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			var n int
			_, _ = fmt.Sscanf(page, "%d", &n)
			assert.LessOrEqual(t, n, 3)
			first := (n-1)*2 + 1
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"links": {"next": null, "previous": null},
				"count": 6, "totalPages": 3, "pageSize": 2, "page": %d,
				"results": [%s, %s]
			}`, n, pagedProductJSON(first), pagedProductJSON(first+1))
		})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("feeds fake django: unexpected call %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	// A sweep is the gateway's own work for the store: it names no
	// visitor, whoever's request started it.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/product/") {
			for _, h := range []string{
				"X-Real-IP", "X-Forwarded-For", "X-Origin-Verify",
			} {
				assert.Empty(t, r.Header.Get(h), "sweep sent %s", h)
			}
			assert.NotEqual(t, crawlerAgent, r.Header.Get("User-Agent"))
		}
		mux.ServeHTTP(w, r)
	})
}

const crawlerAgent = "Feedfetcher-test"

func TestFeedsEndToEnd(t *testing.T) {
	djangoSrv := httptest.NewServer(feedsFakeDjango(t))
	t.Cleanup(djangoSrv.Close)
	rdb := startRedis(t)
	rec := &commandRecorder{}
	rdb.AddHook(rec)

	log := quietLogger()
	metrics := obs.NewMetrics()
	cfg := config.Config{
		Env: config.EnvTest,
		// Registers httptest webhook endpoints on 127.0.0.1.
		AllowLocalWebhooks:      true,
		PaymentHandlerEnv:       config.HandlerEnvSandbox,
		DjangoBaseURL:           djangoSrv.URL + "/api/v1",
		DjangoPublicHost:        "api.example.test",
		AssetsHost:              "assets.platform.test",
		StorefrontDefaultLocale: "el",
		MediaURLTemplate:        "https://{assets_host}/x/{path}",
		FeedImageURLTemplate: "https://{assets_host}/media_stream-image/{path}" +
			"/1000/1000/contain/center/FFFFFF/5/85.jpeg",
		FeedFreshTTL:     time.Hour,
		FeedStaleTTL:     24 * time.Hour,
		TenantCacheTTL:   time.Minute,
		NegativeCacheTTL: time.Minute,
		UpstreamTimeout:  5 * time.Second,
		RateLimitPerMin:  6000,
		RateLimitBurst:   1000,
	}
	dj := django.New(cfg.DjangoBaseURL, cfg.DjangoPublicHost, "test-secret",
		cfg.UpstreamTimeout, log, metrics)
	resolver := tenant.NewResolver(dj, rdb,
		cfg.TenantCacheTTL, cfg.NegativeCacheTTL, log, metrics)
	handler := server.New(server.Deps{
		Cfg: cfg, Log: log, Metrics: metrics, Redis: rdb,
		Django: dj, Resolver: resolver, Version: "test",
		Profiles: newProfiles(t),
	})
	gw := httptest.NewServer(handler)
	t.Cleanup(gw.Close)

	do := func(
		method, path string, headers map[string]string,
	) *http.Response {
		req, err := http.NewRequest(method, gw.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("CF-Connecting-IP", "203.0.113.9")
		req.Header.Set("X-Origin-Verify", "edge-proof")
		req.Header.Set("User-Agent", crawlerAgent)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		// Disable Go's transparent gzip so Content-Encoding is observable.
		req.Header.Set("Accept-Encoding", headers["Accept-Encoding"])
		resp, err := http.DefaultTransport.RoundTrip(req)
		require.NoError(t, err)
		return resp
	}
	get := func(path string, headers map[string]string) *http.Response {
		return do(http.MethodGet, path, headers)
	}

	// Either coding declares its length and sends that many bytes, under
	// one weak validator; HEAD gets the same fields and no content.
	t.Run("both codings declare their length under one weak tag",
		func(t *testing.T) {
			tags := map[string]string{}
			for _, accept := range []string{"gzip", "identity"} {
				resp := get("/feeds/google.xml",
					map[string]string{"Accept-Encoding": accept})
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				_ = resp.Body.Close()
				require.Equal(t, http.StatusOK, resp.StatusCode, accept)
				assert.Equal(t, strconv.Itoa(len(body)),
					resp.Header.Get("Content-Length"), accept)
				assert.Equal(t, "public, max-age=3600",
					resp.Header.Get("Cache-Control"), accept)
				tags[accept] = resp.Header.Get("ETag")
				assert.True(t, strings.HasPrefix(tags[accept], `W/"`), accept)

				head := do(http.MethodHead, "/feeds/google.xml",
					map[string]string{"Accept-Encoding": accept})
				headBody, err := io.ReadAll(head.Body)
				require.NoError(t, err)
				_ = head.Body.Close()
				require.Equal(t, http.StatusOK, head.StatusCode, accept)
				assert.Empty(t, headBody, accept)
				for _, field := range []string{
					"Content-Length", "Content-Encoding", "Content-Type",
					"ETag", "Cache-Control", "Vary",
				} {
					assert.Equal(t, resp.Header.Get(field),
						head.Header.Get(field), accept+" "+field)
				}
			}
			assert.Equal(t, tags["gzip"], tags["identity"])
		})

	t.Run("meta feed renders all pages", func(t *testing.T) {
		resp := get("/feeds/meta.xml", map[string]string{"Accept-Encoding": "identity"})
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Content-Type"), "application/xml")
		assert.NotEmpty(t, resp.Header.Get("ETag"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		for i := 1; i <= 6; i++ {
			assert.Contains(t, string(body),
				fmt.Sprintf("<g:id>%d</g:id>", i), "product %d missing", i)
		}
		assert.Contains(t, string(body), "12.40 EUR")
	})

	t.Run("etag revalidation returns 304", func(t *testing.T) {
		first := get("/feeds/meta.xml", map[string]string{"Accept-Encoding": "identity"})
		_ = first.Body.Close()
		etag := first.Header.Get("ETag")
		require.NotEmpty(t, etag)

		resp := get("/feeds/meta.xml", map[string]string{
			"Accept-Encoding": "identity", "If-None-Match": etag,
		})
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	})

	t.Run("gzip negotiated", func(t *testing.T) {
		resp := get("/feeds/google.xml", map[string]string{"Accept-Encoding": "gzip"})
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
		zr, err := gzip.NewReader(resp.Body)
		require.NoError(t, err)
		body, err := io.ReadAll(zr)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(body),
			`<?xml version="1.0" encoding="UTF-8"?>`))
	})

	t.Run("acp feed serves json", func(t *testing.T) {
		resp := get("/feeds/acp.json", map[string]string{"Accept-Encoding": "identity"})
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(body), `"amount":1240`)
	})

	t.Run("gzip refused with q=0 gets identity", func(t *testing.T) {
		resp := get("/feeds/google.xml",
			map[string]string{"Accept-Encoding": "gzip;q=0, br"})
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Content-Encoding"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(body), `<?xml`))
	})

	// Unlisted identity weighs nothing against gzip at any weight.
	t.Run("gzip at a low weight is still gzip", func(t *testing.T) {
		resp := get("/feeds/google.xml",
			map[string]string{"Accept-Encoding": "gzip;q=0.5"})
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	})

	t.Run("no acceptable coding still gets identity", func(t *testing.T) {
		resp := get("/feeds/google.xml",
			map[string]string{"Accept-Encoding": "br, identity;q=0, *;q=0"})
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Content-Encoding"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(body), `<?xml`))
	})

	// A revalidation reads the metadata and touches the blob, never reads
	// the blob itself; the tag weak or strong (If-None-Match compares
	// weakly), listed, and "*" match.
	t.Run("weak and listed etags revalidate without reading the blob",
		func(t *testing.T) {
			first := get("/feeds/tiktok.xml",
				map[string]string{"Accept-Encoding": "gzip"})
			_ = first.Body.Close()
			etag := first.Header.Get("ETag")
			require.NotEmpty(t, etag)

			for _, inm := range []string{
				etag, strings.TrimPrefix(etag, "W/"), `"stale", ` + etag, "*",
			} {
				rec.start()
				resp := get("/feeds/tiktok.xml", map[string]string{
					"Accept-Encoding": "gzip", "If-None-Match": inm,
				})
				_ = resp.Body.Close()
				assert.Equal(t, http.StatusNotModified, resp.StatusCode, inm)
				assert.Equal(t, etag, resp.Header.Get("ETag"), inm)
				assert.Equal(t, "Accept-Encoding", resp.Header.Get("Vary"), inm)
				assert.Empty(t, rec.readsOf("ag:demostore:feed:rss"), inm)
				// TOUCH, not EXISTS: the blob's LRU clock moves with the
				// meta key's, so allkeys-lru does not evict it first.
				assert.True(t, rec.touched("ag:demostore:feed:rss"), inm)
			}
		})

	// Redis evicts keys one by one: a meta key whose blob is gone is a
	// miss, regenerated at once, never a 503 until the entry goes stale.
	t.Run("an evicted blob is regenerated", func(t *testing.T) {
		first := get("/feeds/tiktok.xml",
			map[string]string{"Accept-Encoding": "gzip"})
		_ = first.Body.Close()
		etag := first.Header.Get("ETag")

		require.NoError(t, rdb.Unlink(t.Context(),
			"ag:demostore:feed:rss").Err())
		resp := get("/feeds/tiktok.xml", map[string]string{
			"Accept-Encoding": "identity", "If-None-Match": etag,
		})
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotModified, resp.StatusCode)
		assert.EqualValues(t, 1,
			rdb.Exists(t.Context(), "ag:demostore:feed:rss").Val(),
			"the revalidation regenerated the blob")

		require.NoError(t, rdb.Unlink(t.Context(),
			"ag:demostore:feed:rss").Err())
		resp = get("/feeds/tiktok.xml",
			map[string]string{"Accept-Encoding": "identity"})
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(body), "<g:id>6</g:id>")
	})

	// A blob that is not this feed's gzip is a miss like an evicted one:
	// it is regenerated, never answered with an error or served broken.
	t.Run("a blob that is not the feed's gzip is regenerated", func(t *testing.T) {
		first := get("/feeds/google.xml",
			map[string]string{"Accept-Encoding": "identity"})
		_ = first.Body.Close()
		require.Equal(t, http.StatusOK, first.StatusCode)

		for _, accept := range []string{"gzip", "identity"} {
			require.NoError(t, rdb.Set(t.Context(), "ag:demostore:feed:rss",
				"not gzip", 0).Err())
			resp := get("/feeds/google.xml",
				map[string]string{"Accept-Encoding": accept})
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode, accept)
			assert.Equal(t, strconv.Itoa(len(body)),
				resp.Header.Get("Content-Length"), accept)
			if accept == "gzip" {
				zr, err := gzip.NewReader(bytes.NewReader(body))
				require.NoError(t, err, accept)
				body, err = io.ReadAll(zr)
				require.NoError(t, err, accept)
			}
			assert.Contains(t, string(body), "<g:id>6</g:id>", accept)
			assert.Equal(t, first.Header.Get("ETag"),
				resp.Header.Get("ETag"), accept)
			assert.NotEqual(t, "not gzip", rdb.Get(t.Context(),
				"ag:demostore:feed:rss").Val(), accept)
		}
	})
}

// commandRecorder records the keys each Redis command named while on.
type commandRecorder struct {
	mu   sync.Mutex
	on   bool
	cmds [][]any
}

func (c *commandRecorder) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.on, c.cmds = true, nil
}

// touched reports whether a TOUCH named key.
func (c *commandRecorder) touched(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, args := range c.cmds {
		if fmt.Sprint(args[0]) == "touch" && fmt.Sprint(args[1]) == key {
			return true
		}
	}
	return false
}

// readsOf returns the commands that named key, other than TOUCH, which
// returns no value.
func (c *commandRecorder) readsOf(key string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var reads []string
	for _, args := range c.cmds {
		if fmt.Sprint(args[0]) == "touch" {
			continue
		}
		for _, a := range args[1:] {
			if fmt.Sprint(a) == key {
				reads = append(reads, fmt.Sprint(args...))
			}
		}
	}
	return reads
}

func (c *commandRecorder) record(cmds ...redis.Cmder) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.on {
		return
	}
	for _, cmd := range cmds {
		c.cmds = append(c.cmds, cmd.Args())
	}
}

func (c *commandRecorder) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (c *commandRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.record(cmd)
		return next(ctx, cmd)
	}
}

func (c *commandRecorder) ProcessPipelineHook(
	next redis.ProcessPipelineHook,
) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		c.record(cmds...)
		return next(ctx, cmds)
	}
}

//go:build integration

package integration

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		"price": 10.00,
		"vatValue": 2.40,
		"finalPrice": 12.40,
		"discountPercent": 0.0,
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
			serveFixture(t, w, "tenant_resolve_webside.json")
		})
	mux.HandleFunc("GET /api/v1/product/category/all",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":1,"translations":{"el":{"name":"Κατηγορία"}},"slug":"cat","active":true,"parent":null,"level":0,"treeId":1}]`))
		})
	// 3 pages x 2 products, exercising the concurrent paged sweep.
	mux.HandleFunc("GET /api/v1/product",
		func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			var n int
			_, _ = fmt.Sscanf(page, "%d", &n)
			require.LessOrEqual(t, n, 3)
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
	return mux
}

func TestFeedsEndToEnd(t *testing.T) {
	djangoSrv := httptest.NewServer(feedsFakeDjango(t))
	t.Cleanup(djangoSrv.Close)
	rdb := startRedis(t)

	log := quietLogger()
	metrics := obs.NewMetrics()
	cfg := config.Config{
		// Registers httptest webhook endpoints on 127.0.0.1.
		Env:              "test",
		DjangoBaseURL:    djangoSrv.URL + "/api/v1",
		DjangoPublicHost: "api.example.test",
		AssetsHost:       "assets.platform.test",
		MediaURLTemplate: "https://{assets_host}/x/{path}",
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
	})
	gw := httptest.NewServer(handler)
	t.Cleanup(gw.Close)

	get := func(path string, headers map[string]string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, gw.URL+path, nil)
		require.NoError(t, err)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		// Disable Go's transparent gzip so Content-Encoding is observable.
		req.Header.Set("Accept-Encoding", headers["Accept-Encoding"])
		resp, err := http.DefaultTransport.RoundTrip(req)
		require.NoError(t, err)
		return resp
	}

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
		assert.Contains(t, string(body), `"amount": 1240`)
	})
}

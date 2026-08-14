package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/server"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "fixtures", "django", name)
}

func serveFixture(t *testing.T, w http.ResponseWriter, name string) {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// fakeDjangoMux serves recorded fixtures for the endpoints the MCP tools
// consume. tenant/resolve accepts every domain except unknown.test so the
// gateway under test resolves whatever Host httptest assigns.
func fakeDjangoMux(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tenant/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("domain") == "unknown.test" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail": "Store not found."}`))
				return
			}
			serveFixture(t, w, "tenant_resolve_webside.json")
		})
	route := func(pattern, fixture string) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
			serveFixture(t, w, fixture)
		})
	}
	route("GET /api/v1/search/product", "search_product.json")
	route("GET /api/v1/search/trending", "search_trending.json")
	route("GET /api/v1/product/1", "product_detail.json")
	route("GET /api/v1/product/1/variants", "product_variants.json")
	route("GET /api/v1/product/1/reviews", "product_reviews.json")
	route("GET /api/v1/product/category/all", "categories_all.json")
	route("GET /api/v1/pay_way", "pay_way.json")
	route("GET /api/v1/shipping/options", "shipping_options.json")
	route("GET /api/v1/shipping/free-shipping-info", "free_shipping_info.json")
	route("GET /api/v1/shipping/acs/stations/nearest", "acs_stations_nearest.json")
	route("POST /api/v1/shipping/boxnow/lockers/nearest", "boxnow_nearest.json")
	// The e2e suite probes this id to exercise the not-found tool error.
	mux.HandleFunc("GET /api/v1/product/999999",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail": "Not found."}`))
		})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fake django: unexpected call %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

// startGateway boots the full HTTP stack (middleware chain included) against
// the fake Django. Redis is unreachable on purpose: the resolver degrades to
// memory+direct, which is the documented failure mode.
func startGateway(t *testing.T) *httptest.Server {
	t.Helper()
	djangoSrv := httptest.NewServer(fakeDjangoMux(t))
	t.Cleanup(djangoSrv.Close)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError + 1,
	}))
	metrics := obs.NewMetrics()
	cfg := config.Config{
		DjangoBaseURL:    djangoSrv.URL + "/api/v1",
		DjangoPublicHost: "api.example.test",
		MediaURLTemplate: "https://assets.{domain}/media_stream-image/{path}" +
			"/800/800/contain/entropy/transparent/5/80.webp",
		TenantCacheTTL:   time.Minute,
		NegativeCacheTTL: time.Minute,
		UpstreamTimeout:  5 * time.Second,
		RateLimitPerMin:  6000,
		RateLimitBurst:   1000,
	}
	dj := django.New(cfg.DjangoBaseURL, cfg.DjangoPublicHost,
		cfg.UpstreamTimeout, log, metrics)
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond,
		ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond,
		MaxRetries: -1,
	})
	resolver := tenant.NewResolver(dj, rdb,
		cfg.TenantCacheTTL, cfg.NegativeCacheTTL, log, metrics)

	handler := server.New(server.Deps{
		Cfg: cfg, Log: log, Metrics: metrics, Redis: rdb,
		Django: dj, Resolver: resolver, Version: "test",
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func connectMCP(t *testing.T, gatewayURL string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{
		Name: "e2e-test-client", Version: "test",
	}, nil)
	session, err := client.Connect(context.Background(),
		&mcp.StreamableClientTransport{
			Endpoint:             gatewayURL + "/mcp",
			DisableStandaloneSSE: true,
		}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callTool(
	t *testing.T, s *mcp.ClientSession, name string, args map[string]any,
) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name: name, Arguments: args,
	})
	require.NoError(t, err)
	return res
}

func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestMCPEndToEnd(t *testing.T) {
	gw := startGateway(t)
	session := connectMCP(t, gw.URL)

	t.Run("lists all tools", func(t *testing.T) {
		res, err := session.ListTools(context.Background(), nil)
		require.NoError(t, err)
		names := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		assert.ElementsMatch(t, []string{
			"search_products", "get_product", "list_categories",
			"get_trending_searches", "get_product_reviews",
			"get_shipping_options", "find_pickup_points",
			"get_payment_methods",
		}, names)
	})

	t.Run("search_products propagates tenant and maps hits", func(t *testing.T) {
		res := callTool(t, session, "search_products",
			map[string]any{"query": "phone"})
		require.False(t, res.IsError)
		out := structured(t, res)
		assert.EqualValues(t, 123270, out["totalHits"])
		products := out["products"].([]any)
		require.Len(t, products, 2)
		first := products[0].(map[string]any)
		// The exposed id must be the master product id, not the
		// translation row id.
		assert.EqualValues(t, 510, first["id"])
		assert.Equal(t, "934.61", first["finalPrice"])
		assert.Equal(t, "EUR", first["currency"])
		assert.Contains(t, first["url"], "/products/510/")
	})

	t.Run("get_product includes stock and image URL", func(t *testing.T) {
		res := callTool(t, session, "get_product",
			map[string]any{"productId": 1})
		require.False(t, res.IsError)
		out := structured(t, res)
		assert.Equal(t, "464.68", out["finalPrice"])
		assert.EqualValues(t, 186, out["stock"])
		assert.Equal(t, true, out["inStock"])
		assert.Equal(t, "20.0", out["vatPercent"])
	})

	t.Run("get_product unknown id returns tool error", func(t *testing.T) {
		res := callTool(t, session, "get_product",
			map[string]any{"productId": 999999})
		require.True(t, res.IsError)
	})

	t.Run("list_categories returns flat hierarchy", func(t *testing.T) {
		res := callTool(t, session, "list_categories", nil)
		require.False(t, res.IsError)
		out := structured(t, res)
		cats := out["categories"].([]any)
		require.NotEmpty(t, cats)
		first := cats[0].(map[string]any)
		assert.Equal(t, "Bags & Luggage", first["name"])
	})

	t.Run("find_pickup_points merges providers", func(t *testing.T) {
		res := callTool(t, session, "find_pickup_points", map[string]any{
			"postalCode": "10434",
			"city":       "Αθήνα",
			"street":     "Σταδίου 1",
		})
		require.False(t, res.IsError)
		out := structured(t, res)
		points := out["points"].([]any)
		require.NotEmpty(t, points)
		providers := map[string]bool{}
		for _, p := range points {
			providers[p.(map[string]any)["provider"].(string)] = true
		}
		assert.True(t, providers["acs"], "ACS points expected")
		assert.True(t, providers["boxnow"], "BOX NOW point expected")
	})

	t.Run("get_payment_methods localizes labels", func(t *testing.T) {
		res := callTool(t, session, "get_payment_methods", nil)
		require.False(t, res.IsError)
		out := structured(t, res)
		methods := out["methods"].([]any)
		require.Len(t, methods, 2)
		labels := []string{
			methods[0].(map[string]any)["label"].(string),
			methods[1].(map[string]any)["label"].(string),
		}
		assert.Contains(t, labels, "VIVA_WALLET")
	})

	t.Run("reviews expose only first name", func(t *testing.T) {
		res := callTool(t, session, "get_product_reviews",
			map[string]any{"productId": 1})
		require.False(t, res.IsError)
		raw, err := json.Marshal(res.StructuredContent)
		require.NoError(t, err)
		// The upstream fixture contains reviewer email/phone/address;
		// none of it may survive the gateway.
		assert.NotContains(t, string(raw), "example.com")
		assert.NotContains(t, string(raw), "phone")
		assert.Contains(t, string(raw), "Willie")
	})
}

func TestMCPUnknownHost404(t *testing.T) {
	gw := startGateway(t)

	req, err := http.NewRequest(http.MethodPost, gw.URL+"/mcp", nil)
	require.NoError(t, err)
	req.Host = "unknown.test"
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

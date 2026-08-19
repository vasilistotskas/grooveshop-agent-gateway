package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
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
			// Real Django only includes chatApiKey for internally
			// authenticated resolves — the gateway must identify itself.
			// Suites construct the client with one of two secrets
			// (the UCP suite's constant is behind the integration tag,
			// so its value is spelled out here).
			got := r.Header.Get("X-Internal-Token")
			if got != "test-secret" && got != "e2e-internal-secret" {
				t.Errorf("tenant/resolve missing internal token, got %q", got)
			}
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

	// Cart endpoints: an anonymous GET mints the empty cart; a GET with
	// X-Cart-Id returns the populated one. Mutations return fixtures.
	mux.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Cart-Id") == "" {
				serveFixture(t, w, "cart_empty.json")
				return
			}
			serveFixture(t, w, "cart_with_items.json")
		})
	route("POST /api/v1/cart/item", "cart_item_created.json")
	route("PUT /api/v1/cart/item/410", "cart_item_created.json")
	mux.HandleFunc("DELETE /api/v1/cart/item/410",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	route("GET /api/v1/order/uuid/b9be45e5-6062-4976-ae7b-2c31eb2ad689",
		"order_by_uuid.json")

	// Agent surface: OIDC-bearer-only endpoints. "Bearer linked-token"
	// is the valid credential; anything else is rejected the way
	// allauth.idp's TokenAuthentication would (401).
	agentRoute := func(pattern, fixture string) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer linked-token" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"detail": "Invalid token."}`))
				return
			}
			serveFixture(t, w, fixture)
		})
	}
	agentRoute("GET /api/v1/agent/me", "agent_me.json")
	agentRoute("GET /api/v1/agent/me/orders", "agent_orders.json")
	agentRoute("GET /api/v1/agent/me/loyalty", "agent_loyalty.json")
	agentRoute("GET /api/v1/agent/me/favourites", "agent_favourites.json")

	// Product alerts: first subscription succeeds, repeats conflict.
	var alertSubscribed atomic.Bool
	mux.HandleFunc("POST /api/v1/product/alert",
		func(w http.ResponseWriter, _ *http.Request) {
			if alertSubscribed.Swap(true) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(
					`{"detail": "Alert already exists."}`))
				return
			}
			serveFixture(t, w, "product_alert_created.json")
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
		// Registers httptest webhook endpoints on 127.0.0.1.
		Env:              "test",
		DjangoBaseURL:    djangoSrv.URL + "/api/v1",
		DjangoPublicHost: "api.example.test",
		AssetsHost:       "assets.platform.test",
		MediaURLTemplate: "https://{assets_host}/media_stream-image/{path}" +
			"/800/800/contain/entropy/transparent/5/80.webp",
		TenantCacheTTL:   time.Minute,
		NegativeCacheTTL: time.Minute,
		UpstreamTimeout:  5 * time.Second,
		RateLimitPerMin:  6000,
		RateLimitBurst:   1000,
	}
	dj := django.New(cfg.DjangoBaseURL, cfg.DjangoPublicHost, "test-secret",
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
			"get_payment_methods", "create_cart", "get_cart",
			"add_to_cart", "update_cart_item", "remove_cart_item",
			"get_checkout_link", "track_order", "subscribe_product_alert",
			"create_checkout", "update_checkout", "complete_checkout",
			"my_orders", "my_loyalty_points", "my_favourites",
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

	t.Run("add_to_cart creates a cart implicitly", func(t *testing.T) {
		res := callTool(t, session, "add_to_cart", map[string]any{
			"productId": 1, "quantity": 2,
		})
		require.False(t, res.IsError)
		out := structured(t, res)
		assert.Equal(t, "29eb4495-e018-45e7-b59c-6646302bd4ef", out["cartId"])
		assert.EqualValues(t, 2, out["totalItems"])
		assert.Equal(t, "929.36", out["total"])
		items := out["items"].([]any)
		require.Len(t, items, 1)
		line := items[0].(map[string]any)
		assert.EqualValues(t, 410, line["itemId"])
		assert.EqualValues(t, 1, line["productId"])
	})

	t.Run("get_checkout_link builds the claim URL", func(t *testing.T) {
		res := callTool(t, session, "get_checkout_link", map[string]any{
			"cartId": "29eb4495-e018-45e7-b59c-6646302bd4ef",
		})
		require.False(t, res.IsError)
		out := structured(t, res)
		assert.Contains(t, out["url"],
			"/cart/claim?uuid=29eb4495-e018-45e7-b59c-6646302bd4ef")
	})

	t.Run("track_order reports status without recipient PII", func(t *testing.T) {
		res := callTool(t, session, "track_order", map[string]any{
			"orderUuid": "b9be45e5-6062-4976-ae7b-2c31eb2ad689",
		})
		require.False(t, res.IsError)
		out := structured(t, res)
		assert.Equal(t, "PROCESSING", out["status"])
		tracking := out["tracking"].(map[string]any)
		assert.Equal(t, "acs", tracking["carrier"])
		assert.Contains(t, tracking["url"], "acscourier.net")

		// The upstream payload carries the recipient's email, phone and
		// address — none may survive the gateway.
		raw, err := json.Marshal(res.StructuredContent)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "@example")
		assert.NotContains(t, string(raw), "zipcode")
	})

	t.Run("subscribe_product_alert handles duplicates", func(t *testing.T) {
		args := map[string]any{
			"productId": 3, "kind": "restock",
			"email": "shopper@example.com",
		}
		res := callTool(t, session, "subscribe_product_alert", args)
		require.False(t, res.IsError)
		out := structured(t, res)
		assert.Equal(t, true, out["subscribed"])
		assert.Equal(t, false, out["alreadySubscribed"])

		res = callTool(t, session, "subscribe_product_alert", args)
		require.False(t, res.IsError,
			"a duplicate subscription is a satisfied outcome")
		out = structured(t, res)
		assert.Equal(t, true, out["alreadySubscribed"])
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

// bearerTransport injects a static Authorization header into every MCP
// HTTP request, the way an OAuth-linked agent's client would.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connectMCPWithBearer(
	t *testing.T, gatewayURL, token string,
) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{
		Name: "e2e-linked-client", Version: "test",
	}, nil)
	session, err := client.Connect(context.Background(),
		&mcp.StreamableClientTransport{
			Endpoint:             gatewayURL + "/mcp",
			HTTPClient:           &http.Client{Transport: bearerTransport{token}},
			DisableStandaloneSSE: true,
		}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestMCPAccountTools(t *testing.T) {
	gw := startGateway(t)

	t.Run("anonymous my_orders explains the OAuth link", func(t *testing.T) {
		session := connectMCP(t, gw.URL)
		res := callTool(t, session, "my_orders", map[string]any{})
		require.True(t, res.IsError)
		text := res.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "oauth-protected-resource/mcp")
	})

	t.Run("linked my_orders returns the shopper's orders", func(t *testing.T) {
		session := connectMCPWithBearer(t, gw.URL, "linked-token")
		res := callTool(t, session, "my_orders", map[string]any{})
		require.False(t, res.IsError)
		out := structured(t, res)
		orders := out["orders"].([]any)
		require.Len(t, orders, 2)
		first := orders[0].(map[string]any)
		assert.Equal(t, "b9be45e5-6062-4976-ae7b-2c31eb2ad689",
			first["orderUuid"])
		assert.Equal(t, "929.36", first["itemsTotal"])
	})

	t.Run("linked my_favourites returns the shopper's products", func(t *testing.T) {
		session := connectMCPWithBearer(t, gw.URL, "linked-token")
		res := callTool(t, session, "my_favourites", map[string]any{})
		require.False(t, res.IsError)
		out := structured(t, res)
		favs := out["favourites"].([]any)
		require.Len(t, favs, 2)
		first := favs[0].(map[string]any)
		assert.EqualValues(t, 6125, first["productId"])
		assert.Equal(t, "476.35", first["finalPrice"])
		assert.Equal(t, true, first["inStock"])
	})

	t.Run("linked my_loyalty_points localizes the tier", func(t *testing.T) {
		session := connectMCPWithBearer(t, gw.URL, "linked-token")
		res := callTool(t, session, "my_loyalty_points", map[string]any{})
		require.False(t, res.IsError)
		out := structured(t, res)
		assert.Equal(t, "420", out["pointsBalance"])
		assert.Equal(t, "Χρυσό", out["tier"])
	})

	t.Run("invalid bearer is challenged with 401 + RFC 9728", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, gw.URL+"/mcp", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer expired-token")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("WWW-Authenticate"),
			"oauth-protected-resource/mcp")
	})
}

package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

const emptyOrderUUID = "4f0c2a8e-6b1d-4c3e-9f7a-0d5e2b8c1a36"

// emptyStoreDjango answers every call the tools make as a store with
// nothing to list: no products, categories, reviews, options, methods,
// order lines or account rows.
func emptyStoreDjango(t *testing.T) *django.Client {
	t.Helper()
	cart := `{"uuid": "` + fixtureCartID + `", "items": []}`
	item := `{"id": 1, "cartId": "` + fixtureCartID + `", ` +
		`"product": {"id": 1}, "quantity": 1}`
	answers := map[string]string{
		"GET /api/v1/search/product":              `{"estimatedTotalHits": 0, "results": []}`,
		"GET /api/v1/search/trending":             `{"windowHours": 24, "results": []}`,
		"GET /api/v1/product/1":                   `{"id": 1, "active": true}`,
		"GET /api/v1/product/1/variants":          `{"axes": [], "variants": []}`,
		"GET /api/v1/product/1/reviews":           `{"count": 0, "results": []}`,
		"GET /api/v1/product/category/all":        `[]`,
		"GET /api/v1/shipping/options":            `[]`,
		"GET /api/v1/shipping/free-shipping-info": `{"providers": []}`,
		"GET /api/v1/pay_way":                     `{"count": 0, "results": []}`,
		"GET /api/v1/settings/public": `{"settings": {"` +
			django.SettingReturnWindowDays + `": "0", "` +
			django.SettingReturnFees + `": ""}}`,
		"GET /api/v1/cart":           cart,
		"POST /api/v1/cart/item":     item,
		"PUT /api/v1/cart/item/1":    item,
		"POST /api/v1/cart/coupon":   cart,
		"DELETE /api/v1/cart/coupon": cart,
		"GET /api/v1/order/uuid/" + emptyOrderUUID: `{"uuid": "` +
			emptyOrderUUID + `", "items": []}`,
		"POST /api/v1/product/alert": `{"id": 1, "kind": "restock", ` +
			`"product": 1, "email": "shopper@example.test"}`,
		"GET /api/v1/agent/me/orders":     `[]`,
		"GET /api/v1/agent/me/loyalty":    `{}`,
		"GET /api/v1/agent/me/favourites": `[]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			route := r.Method + " " + r.URL.Path
			switch route {
			case "DELETE /api/v1/cart/item/1":
				w.WriteHeader(http.StatusNoContent)
				return
			case "GET /api/v1/content-page/" + django.ContentPageReturnPolicy:
				w.WriteHeader(http.StatusNotFound)
				return
			}
			body, ok := answers[route]
			if !ok {
				t.Errorf("unexpected call %s", route)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, slog.New(slog.DiscardHandler), nil)
}

// nullsIn lists the paths of every null in a decoded JSON value.
func nullsIn(path string, v any) []string {
	switch v := v.(type) {
	case nil:
		return []string{path}
	case map[string]any:
		var out []string
		for k, member := range v {
			out = append(out, nullsIn(path+"."+k, member)...)
		}
		return out
	case []any:
		var out []string
		for i, member := range v {
			out = append(out, nullsIn(fmt.Sprintf("%s[%d]", path, i),
				member)...)
		}
		return out
	}
	return nil
}

// A list a tool has nothing for is [], never null: an agent iterating
// it must not have to guard against a missing list. Every tool the
// chat server carries (a superset of /mcp's) is called against a store
// with nothing to list; a new tool fails here until it is called too.
func TestToolOutputsListNothingAsEmptyArrays(t *testing.T) {
	args := map[string]map[string]any{
		"search_products":       {"query": "κάτι"},
		"get_product":           {"productId": 1},
		"list_categories":       {},
		"get_trending_searches": {},
		"get_product_reviews":   {"productId": 1},
		"get_shipping_options":  {"countryCode": "GR"},
		"find_pickup_points": {"countryCode": "GR",
			"postalCode": "10434"},
		"get_payment_methods": {},
		"get_store_policies":  {},
		"create_cart":         {},
		"get_cart":            {"cartId": fixtureCartID},
		"add_to_cart":         {"cartId": fixtureCartID, "productId": 1},
		"update_cart_item": {"cartId": fixtureCartID, "itemId": 1,
			"quantity": 1},
		"remove_cart_item":  {"cartId": fixtureCartID, "itemId": 1},
		"get_checkout_link": {"cartId": fixtureCartID},
		"apply_coupon":      {"cartId": fixtureCartID, "code": "SAVE10"},
		"remove_coupon":     {"cartId": fixtureCartID},
		"track_order":       {"orderUuid": emptyOrderUUID},
		"subscribe_product_alert": {"productId": 1, "kind": "restock",
			"email": "shopper@example.test"},
		"my_orders":         {},
		"my_loyalty_points": {},
		"my_favourites":     {},
	}
	// The UCP tools answer with the spec's own checkout and order
	// objects (internal/ucp), whose arrays its builders always fill.
	ucpObjects := map[string]bool{
		"create_checkout": true, "get_checkout": true,
		"update_checkout": true, "complete_checkout": true,
		"cancel_checkout": true, "get_order": true,
	}

	store := cartTenant()
	store.Features = EveryFeature
	ctx := identity.NewContext(tenant.NewContext(context.Background(), store),
		&identity.Linked{Bearer: "linked-token"})
	server := NewChatServer(Deps{Site: testSite, Version: "test",
		Django: emptyStoreDjango(t), Log: slog.New(slog.DiscardHandler)})
	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := server.Connect(ctx, serverT, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(ctx, nil)
	require.NoError(t, err)

	for _, tool := range listed.Tools {
		if ucpObjects[tool.Name] {
			continue
		}
		t.Run(tool.Name, func(t *testing.T) {
			in, ok := args[tool.Name]
			require.True(t, ok, "call the new tool %s here", tool.Name)
			res, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name: tool.Name, Arguments: in,
			})
			require.NoError(t, err)
			require.False(t, res.IsError, "%v", res.Content)
			raw, err := json.Marshal(res.StructuredContent)
			require.NoError(t, err)
			var out any
			require.NoError(t, json.Unmarshal(raw, &out))
			assert.Empty(t, nullsIn("$", out), "%s", raw)
		})
	}
}

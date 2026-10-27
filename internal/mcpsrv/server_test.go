package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// The MCP server tells an agent who it is talking to in the initialize
// response. On a white-label storefront that has to be the MERCHANT:
// the neighbouring surfaces (/.well-known/ucp, the feeds) already
// resolve the merchant name, and a hardcoded platform title made every
// tenant's endpoint introduce itself as the platform.
func TestServerTitleIsPerTenant(t *testing.T) {
	deps := Deps{Version: "test"}

	acme := NewServer(deps, Identity{Name: "test.acme/store", Title: "Acme Store"}, EveryFeature)
	require.NotNil(t, acme)

	other := NewServer(deps, Identity{Name: "test.aurora/store", Title: "Aurora Store"}, EveryFeature)
	require.NotNil(t, other)

	// Distinct instances so their advertised titles cannot collide.
	assert.NotSame(t, acme, other)
}

// A stateless server has no session to send list-changed notifications
// on, and logging is deprecated as of 2026-07-28: advertise tools only.
func TestServerAdvertisesOnlyWhatItHonours(t *testing.T) {
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := NewServer(Deps{Version: "test"},
		Identity{Name: "test.acme/store", Title: "Acme Store"}, EveryFeature).
		Connect(ctx, serverT, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	raw, err := json.Marshal(session.InitializeResult().Capabilities)
	require.NoError(t, err)
	assert.JSONEq(t, `{"tools":{}}`, string(raw))
}

// A store rename must reach initialize without a restart.
func TestServerCacheFollowsTheStoreTitle(t *testing.T) {
	c := &serverCache{deps: Deps{Version: "test"}, servers: map[string]*mcp.Server{}}
	before := c.get("acme", Identity{Name: "test.acme/store", Title: "Acme Store"}, EveryFeature)
	assert.NotSame(t, before, c.get("acme", Identity{Name: "test.acme/store", Title: "Acme Outlet"}, EveryFeature))
}

func TestServerCacheReusesPerSchema(t *testing.T) {
	c := &serverCache{deps: Deps{Version: "test"}, servers: map[string]*mcp.Server{}}

	first := c.get("acme", Identity{Name: "test.acme/store", Title: "Acme Store"}, EveryFeature)
	again := c.get("acme", Identity{Name: "test.acme/store", Title: "Acme Store"}, EveryFeature)
	assert.Same(t, first, again, "same schema must reuse its server")

	other := c.get("aurora", Identity{Name: "test.aurora/store", Title: "Aurora Store"}, EveryFeature)
	assert.NotSame(t, first, other, "different schema gets its own server")
}

func TestServerCacheIsBounded(t *testing.T) {
	c := &serverCache{deps: Deps{Version: "test"}, servers: map[string]*mcp.Server{}}
	for i := range maxCachedServers + 5 {
		c.get(fmt.Sprintf("tenant_%d", i), Identity{Name: "test/store", Title: "Store"}, EveryFeature)
	}
	assert.LessOrEqual(t, len(c.servers), maxCachedServers)
}

// toolsListed connects a client to a store's server and lists its tools.
func toolsListed(t *testing.T, features django.StoreFeatures) []string {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := NewServer(Deps{Version: "test"},
		Identity{Name: "test.acme/store", Title: "Acme Store"}, features).
		Connect(ctx, serverT, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// A store is served only the tools of the features it has on: one it
// switched off is neither listed nor callable, and Offered (the chat
// bridge's filter) agrees.
func TestStoreIsServedOnlyTheToolsOfItsFeatures(t *testing.T) {
	gated := map[string]func(*django.StoreFeatures){
		"get_product_reviews": func(f *django.StoreFeatures) { f.ProductReviews = false },
		"my_loyalty_points":   func(f *django.StoreFeatures) { f.Loyalty = false },
		"my_favourites":       func(f *django.StoreFeatures) { f.Favourites = false },
		"subscribe_product_alert": func(f *django.StoreFeatures) {
			f.ProductAlerts = false
		},
	}
	every := toolsListed(t, EveryFeature)
	for name, switchOff := range gated {
		t.Run(name, func(t *testing.T) {
			features := EveryFeature
			switchOff(&features)

			listed := toolsListed(t, features)

			assert.Contains(t, every, name)
			assert.NotContains(t, listed, name)
			assert.Len(t, listed, len(every)-1, "only its own tool goes")
			assert.True(t, Offered(name, EveryFeature))
			assert.False(t, Offered(name, features))
		})
	}
}

// The cart and the catalogue each take their whole group of tools, and
// nothing else: a store's past orders stay reachable either way.
func TestCartAndCatalogueSwitchOffTheirTools(t *testing.T) {
	catalogue := []string{
		"search_products", "get_product", "list_categories",
		"get_trending_searches", "get_product_reviews",
		"subscribe_product_alert", "my_favourites",
	}
	cart := []string{
		"get_shipping_options", "find_pickup_points", "get_payment_methods",
		"create_cart", "get_cart", "add_to_cart", "update_cart_item",
		"remove_cart_item", "get_checkout_link", "create_checkout",
		"get_checkout", "update_checkout", "complete_checkout",
		"cancel_checkout",
	}
	orders := []string{"track_order", "my_orders", "get_order", "my_loyalty_points"}

	noCart := EveryFeature
	noCart.Cart = false
	listed := toolsListed(t, noCart)
	for _, name := range cart {
		assert.NotContains(t, listed, name)
		assert.False(t, Offered(name, noCart))
	}
	for _, name := range append(catalogue, orders...) {
		assert.Contains(t, listed, name)
	}
	assert.False(t, Offered("apply_coupon", noCart), "a coupon needs a cart")

	// Django folds the catalogue into the cart, so a store without its
	// catalogue has neither.
	noCatalogue := EveryFeature
	noCatalogue.Catalogue = false
	noCatalogue.Cart = false
	listed = toolsListed(t, noCatalogue)
	for _, name := range append(catalogue, cart...) {
		assert.NotContains(t, listed, name)
	}
	for _, name := range orders {
		assert.Contains(t, listed, name)
	}
}

// A feature switched on or off must reach tools/list without a restart.
func TestServerCacheFollowsTheStoreFeatures(t *testing.T) {
	c := &serverCache{deps: Deps{Version: "test"}, servers: map[string]*mcp.Server{}}
	id := Identity{Name: "test.acme/store", Title: "Acme Store"}
	noReviews := EveryFeature
	noReviews.ProductReviews = false

	assert.NotSame(t, c.get("acme", id, EveryFeature), c.get("acme", id, noReviews))
}

// EveryFeature is the chat bridge's full toolset: a feature added to
// StoreFeatures must be switched on there too.
func TestEveryFeatureTurnsOnEveryFeature(t *testing.T) {
	v := reflect.ValueOf(EveryFeature)
	for i := range v.NumField() {
		assert.True(t, v.Field(i).Bool(), v.Type().Field(i).Name)
	}
}

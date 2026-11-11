package mcpsrv

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listed connects to srv and returns its tools/list answer.
func listed(t *testing.T, srv *mcp.Server) *mcp.ListToolsResult {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := srv.Connect(ctx, serverT, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	res, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	return res
}

// ucpTools are the UCP tools: the checkout capability's five and the
// order capability's get_order.
var ucpTools = []string{
	"create_checkout", "get_checkout", "update_checkout",
	"complete_checkout", "cancel_checkout", "get_order",
}

// A model cannot supply `meta.ucp-agent.profile`, so the chat server
// carries no tool whose input requires `meta`: not the UCP tools, which
// /mcp serves. Nor the account tools, whose bearer a chat shopper does
// not carry, nor create_cart, whose cart the widget would never adopt.
func TestChatServerListsNoToolThatNeedsMeta(t *testing.T) {
	chat := listedSchemas(t, NewChatServer(Deps{Version: "test"}))
	mcpTools := listedSchemas(t, NewServer(Deps{Version: "test"},
		Identity{Name: "test.acme/store", Title: "Acme Store"}, EveryFeature))

	for name, schema := range chat {
		assert.False(t, requires(schema, "meta"), name)
	}
	for _, name := range ucpTools {
		require.Contains(t, mcpTools, name)
		assert.True(t, requires(mcpTools[name], "meta"), name)
	}
	for _, name := range append([]string{"create_cart", "my_orders",
		"my_loyalty_points", "my_favourites"}, ucpTools...) {
		assert.NotContains(t, chat, name)
		assert.Contains(t, mcpTools, name)
		assert.Contains(t, toolNamesFor(EveryFeature), name,
			"discovery advertises what /mcp serves")
	}
}

// The chat adopts the cart named by the tools that change it, and only
// those, all of which the chat serves.
func TestChangesCartNamesTheChatsCartTools(t *testing.T) {
	var changing []string
	for _, tool := range definitions() {
		if ChangesCart(tool.def.Name) {
			changing = append(changing, tool.def.Name)
			assert.True(t, tool.servedOn(chatSurface), tool.def.Name)
		}
	}
	assert.ElementsMatch(t, []string{"add_to_cart", "update_cart_item",
		"remove_cart_item", "apply_coupon", "remove_coupon"}, changing)
	assert.False(t, ChangesCart("unknown_tool"))
}

// Every shopper tool has a title and says what it does to the store: an
// absent hint means destructive and open-world, and a host would ask the
// shopper before every search. A store's list may be kept for the TTL.
func TestEveryToolIsAnnotatedAndTheListCacheable(t *testing.T) {
	const ttl = 5 * time.Minute
	d := Deps{Version: "test", ListTTL: ttl}
	for surface, srv := range map[string]*mcp.Server{
		"mcp": NewServer(d, Identity{Name: "test.acme/store"},
			EveryFeature),
		"chat": NewChatServer(d),
	} {
		t.Run(surface, func(t *testing.T) {
			res := listed(t, srv)

			assert.Equal(t, int(ttl.Milliseconds()), res.TTLMs)
			assert.Equal(t, "public", res.CacheScope)
			require.NotEmpty(t, res.Tools)
			for _, tool := range res.Tools {
				assert.NotEmpty(t, tool.Title, tool.Name)
				a := tool.Annotations
				require.NotNil(t, a, tool.Name)
				require.NotNil(t, a.DestructiveHint, tool.Name)
				require.NotNil(t, a.OpenWorldHint, tool.Name)
				assert.False(t, *a.OpenWorldHint, tool.Name)
				if a.ReadOnlyHint {
					assert.False(t, *a.DestructiveHint, tool.Name)
				}
			}
		})
	}
}

// What a tool does to the store, as its hints say it.
func TestToolHintsMatchWhatTheyDo(t *testing.T) {
	adds := []string{"create_cart", "add_to_cart",
		"subscribe_product_alert", "create_checkout"}
	changes := []string{"update_cart_item", "remove_cart_item",
		"apply_coupon", "remove_coupon", "update_checkout",
		"complete_checkout", "cancel_checkout"}
	for _, tool := range definitions() {
		name, a := tool.def.Name, tool.def.Annotations
		switch {
		case slices.Contains(adds, name):
			assert.False(t, a.ReadOnlyHint, name)
			assert.False(t, *a.DestructiveHint, name)
			assert.False(t, a.IdempotentHint, name)
		case slices.Contains(changes, name):
			assert.False(t, a.ReadOnlyHint, name)
			assert.True(t, *a.DestructiveHint, name)
			assert.True(t, a.IdempotentHint, name)
		default:
			assert.True(t, a.ReadOnlyHint, name)
		}
	}
}

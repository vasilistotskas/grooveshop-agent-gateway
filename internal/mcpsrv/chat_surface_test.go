package mcpsrv

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listedSchemas connects to srv and returns each tool's input schema.
func listedSchemas(
	t *testing.T, srv *mcp.Server,
) map[string]map[string]any {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := srv.Connect(ctx, serverT, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	out := map[string]map[string]any{}
	for _, tool := range listed.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)
		var schema map[string]any
		require.NoError(t, json.Unmarshal(raw, &schema))
		out[tool.Name] = schema
	}
	return out
}

func requires(schema map[string]any, prop string) bool {
	req, _ := schema["required"].([]any)
	for _, p := range req {
		if p == prop {
			return true
		}
	}
	return false
}

// cartIDTools name their cart by cartId.
var cartIDTools = []string{
	"get_cart", "update_cart_item", "remove_cart_item", "get_checkout_link",
}

// /mcp agents always name their cart, so the schema they are served
// requires cartId; the coupon tools are the chat's alone, and discovery
// does not advertise them.
func TestMCPServerRequiresCartIDAndOmitsChatTools(t *testing.T) {
	schemas := listedSchemas(t, NewServer(Deps{Version: "test"},
		Identity{Name: "test.acme/store", Title: "Acme Store"}, EveryFeature))

	for _, name := range cartIDTools {
		require.Contains(t, schemas, name)
		assert.True(t, requires(schemas[name], "cartId"), name)
	}
	assert.NotContains(t, schemas, "apply_coupon")
	assert.NotContains(t, schemas, "remove_coupon")
	assert.NotContains(t, toolNamesFor(EveryFeature), "apply_coupon")
	assert.NotContains(t, toolNamesFor(EveryFeature), "remove_coupon")
}

// The chat reaches a signed-in shopper's cart through their token, so
// its schema leaves cartId optional (the handler requires it of a
// guest), and it carries the coupon tools, gated on promotions.
func TestChatServerLeavesCartIDOptionalAndCarriesCoupons(t *testing.T) {
	schemas := listedSchemas(t, NewChatServer(Deps{Version: "test"}))

	for _, name := range cartIDTools {
		require.Contains(t, schemas, name)
		assert.False(t, requires(schemas[name], "cartId"), name)
		assert.Contains(t, schemas[name]["properties"], "cartId", name)
	}
	assert.True(t, requires(schemas["update_cart_item"], "itemId"))
	require.Contains(t, schemas, "apply_coupon")
	assert.True(t, requires(schemas["apply_coupon"], "code"))
	assert.Contains(t, schemas, "remove_coupon")

	noPromotions := EveryFeature
	noPromotions.Promotions = false
	assert.True(t, Offered("apply_coupon", EveryFeature))
	assert.False(t, Offered("apply_coupon", noPromotions))
	assert.False(t, Offered("remove_coupon", noPromotions))
}

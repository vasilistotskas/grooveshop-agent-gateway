//go:build integration

package integration

import (
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// pickupAt is the fulfillment extension's member of a checkout picked up
// at the point the destination id names.
func pickupAt(destinationID string) map[string]any {
	return map[string]any{"methods": []any{map[string]any{
		"type": "pickup", "line_item_ids": []any{"410"},
		"selected_destination_id": destinationID,
	}}}
}

// A UCP checkout picked up at a point: the pickup method holds the
// lines, the point is listed by its catalogue name, and the order goes
// to it with no address. A point the store's catalogue does not hold is
// taken (BOX NOW's live lookup is ahead of a daily sync), named by its
// carrier and id; Django refuses it when the order is sent, the complete
// answers the checkout incomplete with the error at the pickup method's
// selection, and another point places the order.
func TestUCPPickupOrdersGoToThePoint(t *testing.T) {
	stack := startUCPGateway(t)
	platform := httptest.NewServer(newWebhookSink().handler())
	t.Cleanup(platform.Close)
	session := connectMCP(t, stack.gw.URL)
	profile := servePlatformProfile(t, platform.URL+"/ucp/orders", nil)
	meta := func(key string) map[string]any {
		m := map[string]any{"ucp-agent": map[string]any{"profile": profile}}
		if key != "" {
			m["idempotency-key"] = key
		}
		return m
	}
	buyer := map[string]any{
		"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
		"email": "maria@example.test", "phone_number": "+306912345678",
	}
	lines := []any{map[string]any{
		"item": map[string]any{"id": "1"}, "quantity": 2,
	}}
	pickupMethod := func(out map[string]any) map[string]any {
		methods := out["fulfillment"].(map[string]any)["methods"].([]any)
		require.Len(t, methods, 2)
		return methods[1].(map[string]any)
	}
	complete := func(id string) map[string]any {
		res := callTool(t, session, "complete_checkout", map[string]any{
			"meta": meta(uuid.NewString()), "id": id,
			"checkout": map[string]any{"payment": map[string]any{}},
		})
		require.False(t, res.IsError, "%v", res.Content[0].(*mcp.TextContent).Text)
		return structured(t, res)
	}

	res := callTool(t, session, "create_checkout", map[string]any{
		"meta": meta(""), "cart_id": stack.faults.carts.mint(),
		"checkout": map[string]any{
			"buyer": buyer, "fulfillment": pickupAt("boxnow:GR:999"),
			"payment": codPayment(),
		},
	})
	require.False(t, res.IsError, "%v", res.Content[0].(*mcp.TextContent).Text)
	created := structured(t, res)
	require.Equal(t, "ready_for_complete", created["status"],
		"%v", created["messages"])
	id := created["id"].(string)
	pickup := pickupMethod(created)
	assert.Equal(t, []any{"410"}, pickup["line_item_ids"])
	assert.Equal(t, "boxnow:GR:999", pickup["selected_destination_id"])
	assert.Equal(t, "BOX NOW 999",
		pickup["destinations"].([]any)[0].(map[string]any)["name"],
		"not in the catalogue: named by its carrier and id")
	placed := stack.faults.ordersPlaced.Load()

	answer := complete(id)
	assert.Equal(t, "incomplete", answer["status"])
	assert.Equal(t, placed, stack.faults.ordersPlaced.Load())
	var refused map[string]any
	for _, m := range answer["messages"].([]any) {
		if msg := m.(map[string]any); msg["path"] ==
			"$.fulfillment.methods[1].selected_destination_id" {
			refused = msg
		}
	}
	require.NotNil(t, refused, "%v", answer["messages"])
	assert.Equal(t, "pickup_point_unknown", refused["code"])
	assert.Equal(t, "recoverable", refused["severity"])

	res = callTool(t, session, "update_checkout", map[string]any{
		"meta": meta(""), "id": id,
		"checkout": map[string]any{
			"line_items": lines, "buyer": buyer,
			"fulfillment": pickupAt("boxnow:GR:25"),
		},
	})
	require.False(t, res.IsError, "%v", res.Content[0].(*mcp.TextContent).Text)
	updated := structured(t, res)
	assert.Equal(t, "ready_for_complete", updated["status"])
	point := pickupMethod(updated)["destinations"].([]any)[0].(map[string]any)
	assert.Equal(t, "AB Shop & Go - Εξάρχεια", point["name"],
		"read from the catalogue")
	assert.Equal(t, "Ιπποκράτους 132",
		point["address"].(map[string]any)["street_address"])

	assert.Equal(t, "completed", complete(id)["status"])
	order := stack.faults.lastOrder.Load()
	require.NotNil(t, order)
	assert.Equal(t, checkout.FulfillmentPickupPoint, (*order)["shippingKind"])
	assert.Equal(t, "boxnow", (*order)["shippingProviderCode"])
	assert.Equal(t, "25", (*order)["boxnowLockerId"])
	assert.Equal(t, "GR", (*order)["countryId"])
	assert.NotContains(t, *order, "street")
	assert.NotContains(t, *order, "regionId")

	// A point Django holds in another country than the one its id names
	// is refused as no resync could change: the same answer, its own
	// code, nothing placed.
	res = callTool(t, session, "create_checkout", map[string]any{
		"meta": meta(""), "cart_id": stack.faults.carts.mint(),
		"checkout": map[string]any{
			"buyer": buyer, "fulfillment": pickupAt("boxnow:CY:25"),
			"payment": codPayment(),
		},
	})
	require.False(t, res.IsError, "%v", res.Content[0].(*mcp.TextContent).Text)
	placed = stack.faults.ordersPlaced.Load()
	answer = complete(structured(t, res)["id"].(string))
	assert.Equal(t, "incomplete", answer["status"])
	assert.Equal(t, placed, stack.faults.ordersPlaced.Load())
	refused = nil
	for _, m := range answer["messages"].([]any) {
		if msg := m.(map[string]any); msg["path"] ==
			"$.fulfillment.methods[1].selected_destination_id" {
			refused = msg
		}
	}
	require.NotNil(t, refused, "%v", answer["messages"])
	assert.Equal(t, "pickup_point_in_another_country", refused["code"])
}

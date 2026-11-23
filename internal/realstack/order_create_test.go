//go:build realstack

package realstack

import (
	"fmt"
	"maps"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What Django does at order creation with what a checkout sends, from
// its own code: a street line it cannot split and a cart that is not the
// one the checkout showed are refused, placing nothing, and the checkout
// says what to change; a pickup order goes to the point it names.

// assertOrdersOfCheckout checks how many orders Django keeps for an agent
// checkout.
func assertOrdersOfCheckout(t *testing.T, checkout string, want int) {
	t.Helper()
	managePy(t, fmt.Sprintf(`
from django_tenants.utils import schema_context
from order.models.order import Order
with schema_context("demostore"):
    placed = Order.objects.filter(agent_checkout=%q).count()
    assert placed == %d, placed
`, checkout, want))
}

// messageAt is the checkout message at path, nil when there is none.
func messageAt(payload map[string]any, path string) map[string]any {
	messages, _ := payload["messages"].([]any)
	for _, m := range messages {
		if msg := m.(map[string]any); msg["path"] == path {
			return msg
		}
	}
	return nil
}

// updateWith replaces a checkout of one unit of product with submit.
func updateWith(
	t *testing.T, s *mcp.ClientSession, id, product string,
	submit map[string]any,
) map[string]any {
	t.Helper()
	checkout := map[string]any{"line_items": []any{map[string]any{
		"item": map[string]any{"id": product}, "quantity": 1,
	}}}
	maps.Copy(checkout, submit)
	return structured(t, call(t, s, "update_checkout", map[string]any{
		"meta": meta(""), "id": id, "checkout": checkout,
	}))
}

// Django splits the one street line into its street and house number, and
// refuses a line without a number (street_number_missing). The completion
// places nothing and answers the checkout incomplete with Django's code at
// the street line, which stays until the address changes; then the order
// is placed.
func TestAStreetLineWithoutItsNumberIsRefusedAtTheLine(t *testing.T) {
	const streetPath = "$.fulfillment.methods[0].destinations[0].street_address"
	s := connect(t, "")
	product := productID(t, s)
	created := structured(t, call(t, s, "create_checkout", map[string]any{
		"meta": meta(""), "checkout": map[string]any{
			"line_items": []any{map[string]any{
				"item": map[string]any{"id": product}, "quantity": 1,
			}},
		},
	}))
	id := created["id"].(string)
	submit := payOnDelivery()
	submit["fulfillment"] = deliveryTo("Πανεπιστημίου", "GR")
	updated := updateWith(t, s, id, product, submit)
	require.Equal(t, "ready_for_complete", updated["status"],
		"the gateway leaves the split to Django: %v", updated["messages"])

	refused := structured(t, completeOnDelivery(t, s, id))

	assert.Equal(t, "incomplete", refused["status"])
	msg := messageAt(refused, streetPath)
	require.NotNil(t, msg, "%v", refused["messages"])
	assert.Equal(t, "street_number_missing", msg["code"])
	assertOrdersOfCheckout(t, id, 0)
	read := structured(t, call(t, s, "get_checkout", map[string]any{
		"meta": meta(""), "id": id,
	}))
	assert.Equal(t, "incomplete", read["status"], "kept until it changes")
	assert.NotNil(t, messageAt(read, streetPath))

	fixed := updateWith(t, s, id, product, payOnDelivery())
	require.Equal(t, "ready_for_complete", fixed["status"],
		"%v", fixed["messages"])
	done := structured(t, completeOnDelivery(t, s, id))
	require.Equal(t, "completed", done["status"])
	assertOrdersOfCheckout(t, id, 1)
}

// A checkout's order is placed only from the cart it showed: Django
// compares the cart digest the checkout stored with the cart's own, under
// the cart's lock, and refuses another cart with 409 cart_changed before
// it compares the total. The cart is written in Django, as the storefront
// writes a cart the buyer claimed. Nothing is placed, a read shows and
// stores the cart now, and the next completion places it.
func TestACartWrittenAfterItsCheckoutShowedItIsNotOrdered(t *testing.T) {
	s := connect(t, "")
	cart := cartWith(t, s, productID(t, s))
	created := checkoutOn(t, s, cart, payOnDelivery())
	require.Equal(t, "ready_for_complete", created["status"],
		"%v", created["messages"])
	id := created["id"].(string)
	managePy(t, fmt.Sprintf(`
from django_tenants.utils import schema_context
from cart.models import Cart
with schema_context("demostore"):
    item = Cart.objects.get(uuid=%q).items.get()
    item.quantity = 2
    item.save(update_fields=["quantity"])
`, cart))

	text := errorText(t, completeOnDelivery(t, s, id))

	assert.Contains(t, text, `"code":"cart_changed"`)
	assertOrdersFromCart(t, cart, 0)
	// A retry under another key sends the cart the checkout stored, and
	// is refused again.
	assert.Contains(t, errorText(t, completeOnDelivery(t, s, id)),
		`"code":"cart_changed"`)

	read := structured(t, call(t, s, "get_checkout", map[string]any{
		"meta": meta(""), "id": id,
	}))
	require.Equal(t, "ready_for_complete", read["status"],
		"%v", read["messages"])
	lines := read["line_items"].([]any)
	require.Len(t, lines, 1)
	assert.EqualValues(t, 2, lines[0].(map[string]any)["quantity"],
		"the read shows the cart as it is now")
	done := structured(t, completeOnDelivery(t, s, id))
	require.Equal(t, "completed", done["status"])
	assertOrdersFromCart(t, cart, 1)
}

// pickupAt is the fulfillment extension's member choosing the pickup
// method, its only one, at the point a location id names.
func pickupAt(location string) map[string]any {
	return map[string]any{"methods": []any{map[string]any{
		"type": "pickup", "line_item_ids": []any{},
		"selected_destination_id": location,
	}}}
}

// assertPickupOrder checks the order Django placed for a checkout: a
// pickup at a point of carrier, delivered to the point's address from the
// store's catalogue.
func assertPickupOrder(t *testing.T, checkout, carrier, address string) {
	t.Helper()
	managePy(t, fmt.Sprintf(`
from django_tenants.utils import schema_context
from order.models.order import Order
with schema_context("demostore"):
    order = Order.objects.get(agent_checkout=%q)
    assert order.shipping_kind == "pickup_point", order.shipping_kind
    assert order.shipping_provider.code == %q, order.shipping_provider.code
    placed = f"{order.street} {order.street_number}"
    assert placed == %q, placed
`, checkout, carrier, address))
}

// A pickup order sends Django the point and no address: Django delivers
// to the point's address in its catalogue. The quote carries the same
// point, so the order's cart digest is the one the checkout showed. Each
// carrier's point is selected by the id this business gives it, shown by
// the name the catalogue holds, and ordered to.
func TestUCPPickupOrdersGoToThePoint(t *testing.T) {
	s := connect(t, "")
	product := productID(t, s)
	for _, tc := range []struct {
		name, location, carrier, pointName, address string
		// payment is how the order is paid: ACS takes cash on delivery,
		// BOX NOW never cash (its lockers meet no courier), so its order
		// is paid online and awaits a payment page this stack cannot open.
		payment map[string]any
		placed  func(t *testing.T, res *mcp.CallToolResult)
	}{{
		name: "an ACS Smartpoint", location: "acs:GR:RST:1",
		carrier: "acs", pointName: "Real-stack Smartpoint",
		address: "Πανεπιστημίου 30",
		payment: map[string]any{"payment": cashOnDelivery},
		placed: func(t *testing.T, res *mcp.CallToolResult) {
			require.Equal(t, "completed", structured(t, res)["status"])
		},
	}, {
		name: "a BOX NOW locker", location: "boxnow:GR:9001",
		carrier: "boxnow", pointName: "Real-stack locker",
		address: "Σταδίου 10",
		payment: map[string]any{"pay_way_id": onlinePayWay(t, s)},
		placed: func(t *testing.T, res *mcp.CallToolResult) {
			assert.Contains(t, errorText(t, res),
				"its payment link could not be created")
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			created := structured(t, call(t, s, "create_checkout",
				map[string]any{"meta": meta(""), "checkout": map[string]any{
					"line_items": []any{map[string]any{
						"item": map[string]any{"id": product}, "quantity": 1,
					}},
				}}))
			id := created["id"].(string)
			submit := map[string]any{
				"buyer": buyer, "fulfillment": pickupAt(tc.location),
			}
			maps.Copy(submit, tc.payment)
			updated := updateWith(t, s, id, product, submit)
			require.Equal(t, "ready_for_complete", updated["status"],
				"%v", updated["messages"])
			methods := updated["fulfillment"].(map[string]any)["methods"]
			pickup := methods.([]any)[1].(map[string]any)
			assert.Equal(t, tc.location, pickup["selected_destination_id"])
			shown := false
			for _, d := range pickup["destinations"].([]any) {
				dest := d.(map[string]any)
				if dest["id"] == tc.location {
					shown = true
					assert.Equal(t, tc.pointName, dest["name"])
				}
			}
			assert.True(t, shown, "the selected point is listed")

			// The spec requires `payment` on a complete: none named keeps
			// the checkout's own method.
			tc.placed(t, call(t, s, "complete_checkout", map[string]any{
				"meta": meta(uuid.NewString()), "id": id,
				"checkout": map[string]any{"payment": map[string]any{}},
			}))
			assertPickupOrder(t, id, tc.carrier, tc.address)
		})
	}
}

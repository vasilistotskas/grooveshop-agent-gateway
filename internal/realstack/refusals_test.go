//go:build realstack

package realstack

import (
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What Django refuses at order creation, from its own checks: a street
// line it cannot split, and a cart that is not the one the checkout
// showed. Each places nothing, and the checkout says what to change.

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
	for k, v := range submit {
		checkout[k] = v
	}
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

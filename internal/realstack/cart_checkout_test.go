//go:build realstack

package realstack

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// One live checkout holds a cart. The rule acts only on a create_checkout
// that names a cart, and rests on what Django does with that cart: how it
// reads the cart's id, that it keeps the cart of an order awaiting its
// online payment, what it reports once it canceled that order, and that it
// refuses a second agent order from such a cart itself.

// visitor sends its requests as the store's ingress hands them to the
// gateway: naming the address they came from, which the gateway relays.
type visitor struct {
	address string
	next    http.RoundTripper
}

func (v visitor) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", v.address)
	return v.next.RoundTrip(r)
}

var visitors atomic.Int32

// connectAsVisitor is connect from an address of its own. Django lets one
// address place ten guest orders a minute. With no ingress in the stack
// every test's orders come from the gateway's address, and with the orders
// placed here the suite would place more than ten in its minute.
func connectAsVisitor(t *testing.T) *mcp.ClientSession {
	t.Helper()
	address := fmt.Sprintf("203.0.113.%d", visitors.Add(1))
	return connectVia(t, visitor{address, hostTransport{storeHost, ""}})
}

// cartWith is a cart the cart tools made, holding one unit.
func cartWith(t *testing.T, s *mcp.ClientSession, product string) string {
	t.Helper()
	id, err := strconv.ParseInt(product, 10, 64)
	require.NoError(t, err)
	added := structured(t, call(t, s, "add_to_cart", map[string]any{
		"productId": id,
	}))
	return added["cartId"].(string)
}

// checkoutOn starts a checkout on a cart of the cart tools.
func checkoutOn(
	t *testing.T, s *mcp.ClientSession, cart string, submit map[string]any,
) map[string]any {
	t.Helper()
	return structured(t, call(t, s, "create_checkout", map[string]any{
		"meta": meta(""), "cart_id": cart, "checkout": submit,
	}))
}

func checkoutStatus(t *testing.T, s *mcp.ClientSession, id string) any {
	t.Helper()
	return structured(t, call(t, s, "get_checkout", map[string]any{
		"meta": meta(""), "id": id,
	}))["status"]
}

// payOnDelivery is all a checkout needs to be ready, paid on delivery.
func payOnDelivery() map[string]any {
	return map[string]any{
		"buyer": buyer, "fulfillment": homeDelivery("GR"),
		"payment": cashOnDelivery,
	}
}

// completeOnDelivery completes a checkout that is paid on delivery.
func completeOnDelivery(
	t *testing.T, s *mcp.ClientSession, id string,
) *mcp.CallToolResult {
	t.Helper()
	return call(t, s, "complete_checkout", map[string]any{
		"meta": meta(uuid.NewString()), "id": id,
		"checkout": map[string]any{"payment": cashOnDelivery},
	})
}

// assertOrdersFromCart checks how many orders Django placed from a cart.
func assertOrdersFromCart(t *testing.T, cart string, want int) {
	t.Helper()
	managePy(t, fmt.Sprintf(`
from django_tenants.utils import schema_context
from order.models.order import Order
with schema_context("demostore"):
    placed = Order.objects.filter(
        metadata__contains={"cart_snapshot": {"cart_uuid": %q}}).count()
    assert placed == %d, placed
`, cart, want))
}

// Django reads every spelling of a cart's id as that cart, so two checkouts
// started on two spellings would be two on one cart. The second create
// takes the cart from the first, and the cart places one order.
func TestACartHasOneCheckoutInAnySpellingOfItsID(t *testing.T) {
	s := connectAsVisitor(t)
	cart := cartWith(t, s, productID(t, s))

	// The gateway keys a cart by the canonical spelling of its id. A
	// checkout from line items is keyed by the spelling Django gave its
	// cart, so that has to be the canonical one.
	canonical, err := uuid.Parse(cart)
	require.NoError(t, err)
	require.Equal(t, canonical.String(), cart)
	shouted := strings.ToUpper(cart)
	require.NotEqual(t, cart, shouted)
	read := structured(t, call(t, s, "get_cart", map[string]any{
		"cartId": shouted,
	}))
	require.Equal(t, cart, read["cartId"],
		"Django read another spelling as another cart")
	require.EqualValues(t, 1, read["totalItems"])

	first := checkoutOn(t, s, cart, map[string]any{})
	second := checkoutOn(t, s, shouted, payOnDelivery())
	require.NotEqual(t, first["id"], second["id"])
	require.Equal(t, "ready_for_complete", second["status"],
		"%v", second["messages"])
	assert.Equal(t, "canceled", checkoutStatus(t, s, first["id"].(string)))

	done := structured(t, completeOnDelivery(t, s, second["id"].(string)))
	require.Equal(t, "completed", done["status"])
	assertOrdersFromCart(t, cart, 1)
}

// unpaid is a checkout whose order awaits its online payment, and the cart
// the order was placed from.
type unpaid struct {
	cart, checkout, order string
}

var placedOrder = regexp.MustCompile(`order ([0-9a-f-]{36}) is placed`)

// onlinePayWay is the id of the pay way the store settles on its payment
// provider's page, which the seed switched on.
func onlinePayWay(t *testing.T, s *mcp.ClientSession) any {
	t.Helper()
	methods := structured(t, call(t, s, "get_payment_methods", map[string]any{
		"shippingProviderCode": "flat_rate", "shippingKind": "home_delivery",
	}))["methods"].([]any)
	for _, method := range methods {
		if m := method.(map[string]any); m["settlement"] == "online" {
			return m["id"]
		}
	}
	require.Fail(t, "the store offers no online pay way", "%v", methods)
	return nil
}

// placeUnpaidOrder checks out a cart of the cart tools with the store's
// online pay way. Django places the order and keeps the cart until the
// payment lands. No payment page can be opened in this stack (paymentHosts),
// so the completion answers that the order is placed without one.
func placeUnpaidOrder(t *testing.T, s *mcp.ClientSession) unpaid {
	t.Helper()
	cart := cartWith(t, s, productID(t, s))
	created := checkoutOn(t, s, cart, map[string]any{
		"buyer": buyer, "fulfillment": homeDelivery("GR"),
		"pay_way_id": onlinePayWay(t, s),
	})
	require.Equal(t, "ready_for_complete", created["status"],
		"%v", created["messages"])
	id := created["id"].(string)

	text := errorText(t, call(t, s, "complete_checkout", map[string]any{
		"meta": meta(uuid.NewString()), "id": id,
		"checkout": map[string]any{},
	}))
	require.Contains(t, text, "its payment link could not be created")
	named := placedOrder.FindStringSubmatch(text)
	require.NotNil(t, named, text)
	order := named[1]
	require.Equal(t, "requires_escalation", checkoutStatus(t, s, id))

	managePy(t, fmt.Sprintf(`
from django_tenants.utils import schema_context
from cart.models import Cart
from order.models.order import Order
with schema_context("demostore"):
    assert Order.objects.awaiting_online_payment().filter(uuid=%q).exists()
    assert Cart.objects.get(uuid=%q).items.count() == 1
`, order, cart))
	return unpaid{cart: cart, checkout: id, order: order}
}

// cancelOrder cancels an unpaid order through Django's own service, the
// way its task does when the order's payment window ends.
func cancelOrder(t *testing.T, order string) {
	t.Helper()
	managePy(t, fmt.Sprintf(`
from django_tenants.utils import tenant_context
from order.enum.status import OrderStatus
from order.models.order import Order
from order.services import OrderService
from tenant.models import Tenant
with tenant_context(Tenant.objects.get(schema_name="demostore")):
    OrderService.cancel_order(
        Order.objects.get(uuid=%q),
        reason="the payment window ended",
        refund_payment=False,
        customer_initiated=False,
    )
    assert Order.objects.get(uuid=%q).status == OrderStatus.CANCELED
`, order, order))
}

// The checkout that placed an order awaiting its online payment keeps the
// cart, which Django keeps too: a second checkout could order the same
// lines again. The gateway asks Django for the order, and once Django
// canceled it the cart is free, on both sides.
func TestACartWithAnUnpaidOrderKeepsItsCheckoutUntilTheOrderIsCanceled(
	t *testing.T,
) {
	s := connectAsVisitor(t)
	held := placeUnpaidOrder(t, s)

	t.Run("a second checkout on the cart is refused", func(t *testing.T) {
		text := errorText(t, call(t, s, "create_checkout", map[string]any{
			"meta": meta(""), "cart_id": held.cart,
			"checkout": map[string]any{},
		}))
		assert.Contains(t, text, `"code":"conflict"`)
		assert.Contains(t, text, "which awaits its payment")
		assert.Contains(t, text, "no checkout was created")
		assert.NotContains(t, text, held.checkout)
		assert.NotContains(t, text, held.order)
		assert.Equal(t, "requires_escalation",
			checkoutStatus(t, s, held.checkout))
	})

	t.Run("the order Django canceled frees the cart", func(t *testing.T) {
		cancelOrder(t, held.order)

		next := checkoutOn(t, s, held.cart, payOnDelivery())
		require.Equal(t, "ready_for_complete", next["status"],
			"%v", next["messages"])
		assert.Equal(t, "canceled", checkoutStatus(t, s, held.checkout))

		// Django takes the next order from the cart: a canceled order is
		// not one that awaits its payment there either.
		done := structured(t, completeOnDelivery(t, s, next["id"].(string)))
		require.Equal(t, "completed", done["status"])
		assertOrdersFromCart(t, held.cart, 2)
	})
}

// loseCartRecord deletes the gateway's record of the checkout that holds a
// cart, as Redis does when it evicts the key.
func loseCartRecord(t *testing.T, cart string) {
	t.Helper()
	code, out, err := running.redis.Exec(context.Background(), []string{
		"redis-cli", "-n", gatewayRedisDB, "--raw",
		"DEL", "ag:demostore:checkoutcart:" + cart,
	}, tcexec.Multiplexed())
	require.NoError(t, err)
	output, err := io.ReadAll(out)
	require.NoError(t, err)
	require.Zero(t, code, string(output))
	require.Equal(t, "1", strings.TrimSpace(string(output)),
		"the cart had no record to lose")
}

// Redis can lose a cart's record while the cart's checkout awaits its
// payment, and the gateway then starts a second checkout on the cart.
// Django refuses that checkout's order, and the platform reads why in
// Django's own code.
func TestDjangoRefusesASecondOrderFromACartWhoseOrderIsUnpaid(t *testing.T) {
	s := connectAsVisitor(t)
	held := placeUnpaidOrder(t, s)
	loseCartRecord(t, held.cart)

	second := checkoutOn(t, s, held.cart, payOnDelivery())
	require.Equal(t, "ready_for_complete", second["status"],
		"%v", second["messages"])
	id := second["id"].(string)

	text := errorText(t, completeOnDelivery(t, s, id))
	assert.Contains(t, text, `"code":"cart_has_unpaid_order"`)
	assert.NotContains(t, text, held.order)
	assertOrdersFromCart(t, held.cart, 1)
	assert.Equal(t, "ready_for_complete", checkoutStatus(t, s, id))
}

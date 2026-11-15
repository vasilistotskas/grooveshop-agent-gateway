//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// update_checkout replaces the checkout: the platform sends the whole
// resource. What the response renders is replaced, so an omitted buyer is
// none and, with the discount extension negotiated, omitted codes are no
// codes: the coupon they applied is removed. What the response does not
// render cannot be sent back, so the gateway's own fulfillment member and
// the payment selection are kept when omitted.
func TestAnUpdateReplacesTheCheckout(t *testing.T) {
	stack := startUCPGateway(t)
	stack.faults.promotions.Store(true)
	platform := httptest.NewServer(newWebhookSink().handler())
	t.Cleanup(platform.Close)
	session := connectMCP(t, stack.gw.URL)
	meta := map[string]any{"ucp-agent": map[string]any{
		"profile": servePlatformProfile(t, platform.URL+"/ucp/orders", nil),
	}}
	var removed atomic.Int32
	count := func(r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/v1/cart/coupon" {
			removed.Add(1)
		}
	}
	stack.faults.before.Store(&count)
	t.Cleanup(func() { stack.faults.before.Store(nil) })
	lines := []any{map[string]any{
		"item": map[string]any{"id": "1"}, "quantity": 2,
	}}

	res := callTool(t, session, "create_checkout", map[string]any{
		"meta": meta, "cart_id": stack.faults.carts.mint(),
		"checkout": map[string]any{
			"buyer": map[string]any{
				"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
				"email": "maria@example.test", "phone_number": "+306912345678",
			},
			"fulfillment": homeDelivery(),
			"payment":     codPayment(),
			"discounts":   map[string]any{"codes": []any{"SAVE10"}},
		},
	})
	require.False(t, res.IsError, "%v", res.Content)
	created := structured(t, res)
	require.Equal(t, "ready_for_complete", created["status"])
	require.Equal(t, []any{"SAVE10"},
		created["discounts"].(map[string]any)["codes"])
	id := created["id"].(string)

	res = callTool(t, session, "update_checkout", map[string]any{
		"meta": meta, "id": id,
		"checkout": map[string]any{"line_items": lines},
	})
	require.False(t, res.IsError, "%v", res.Content)
	updated := structured(t, res)

	assert.Equal(t, "incomplete", updated["status"])
	assert.NotContains(t, updated, "buyer", "an omitted buyer is none")
	missing := fmt.Sprint(updated["messages"])
	assert.Contains(t, missing, "buyer (")
	assert.NotContains(t, missing, "fulfillment (",
		"the delivery is kept: the response does not carry it")
	assert.NotContains(t, missing, "payWayId",
		"the payment selection is kept: the response does not carry it")
	assert.NotContains(t, updated, "discounts", "omitted codes are none")
	assert.EqualValues(t, 1, removed.Load(), "the coupon is removed")
	stored := stack.storedSession(t, id)
	assert.Empty(t, stored.DiscountCodes)
	assert.Equal(t, "home_delivery", stored.Fulfillment.Kind)
	assert.Positive(t, stored.PayWayID)

	// A checkout's codes are its cart's, a coupon attached elsewhere
	// included, so every update without codes clears the cart's coupon;
	// Django's removal changes nothing on a cart without one.
	res = callTool(t, session, "update_checkout", map[string]any{
		"meta": meta, "id": id,
		"checkout": map[string]any{"line_items": lines},
	})
	require.False(t, res.IsError, "%v", res.Content)
	assert.EqualValues(t, 2, removed.Load())
}

// A member of an extension the platform did not negotiate is ignored, not
// refused and not applied: codes from a platform that does not declare the
// discount extension change neither the cart nor the codes the checkout
// holds, and its responses carry no `discounts`.
func TestDiscountsWithoutTheExtensionAreIgnored(t *testing.T) {
	stack := startUCPGateway(t)
	stack.faults.promotions.Store(true)
	platform := httptest.NewServer(newWebhookSink().handler())
	t.Cleanup(platform.Close)
	session := connectMCP(t, stack.gw.URL)
	declaring := map[string]any{"ucp-agent": map[string]any{
		"profile": servePlatformProfile(t, platform.URL+"/ucp/orders", nil),
	}}
	silent := map[string]any{"ucp-agent": map[string]any{
		"profile": servePlatformProfile(t, platform.URL+"/ucp/orders",
			func(s string) string {
				return strings.Replace(s, `"dev.ucp.shopping.discount"`,
					`"com.example.unrelated"`, 1)
			}),
	}}
	lines := []any{map[string]any{
		"item": map[string]any{"id": "1"}, "quantity": 2,
	}}

	res := callTool(t, session, "create_checkout", map[string]any{
		"meta": declaring, "cart_id": stack.faults.carts.mint(),
		"checkout": map[string]any{
			"discounts": map[string]any{"codes": []any{"SAVE10"}},
		},
	})
	require.False(t, res.IsError, "%v", res.Content)
	id := structured(t, res)["id"].(string)
	writes := stack.faults.cartWrites.Load()

	for _, codes := range [][]any{{"OTHER5"}, {}, nil} {
		submit := map[string]any{"line_items": lines}
		if codes != nil {
			submit["discounts"] = map[string]any{"codes": codes}
		}
		res = callTool(t, session, "update_checkout", map[string]any{
			"meta": silent, "id": id, "checkout": submit,
		})
		require.False(t, res.IsError, "%v", res.Content)
		out := structured(t, res)
		assert.NotContains(t, out, "discounts", "%v", codes)
		assert.NotContains(t, out["ucp"].(map[string]any)["capabilities"],
			"dev.ucp.shopping.discount")
		assert.Equal(t, writes, stack.faults.cartWrites.Load(), "%v", codes)
		assert.Equal(t, []string{"SAVE10"},
			stack.storedSession(t, id).DiscountCodes, "%v", codes)
	}

	// A store with promotions off declares no discount extension, so its
	// codes are ignored the same way.
	off := startUCPGateway(t)
	offSession := connectMCP(t, off.gw.URL)
	res = callTool(t, offSession, "create_checkout", map[string]any{
		"meta": declaring, "cart_id": off.faults.carts.mint(),
		"checkout": map[string]any{
			"discounts": map[string]any{"codes": []any{"SAVE10"}},
		},
	})
	require.False(t, res.IsError, "%v", res.Content)
	assert.NotContains(t, structured(t, res), "discounts")
	assert.Zero(t, off.faults.cartWrites.Load())
}

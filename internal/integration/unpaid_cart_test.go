//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// djangoUnpaidSentence is the sentence of Django's refusal in
// order_cart_has_unpaid_order.json.
const djangoUnpaidSentence = "An order placed from this cart is already " +
	"waiting for its payment"

// Django refuses an agent's order from a cart whose earlier order still
// awaits its online payment. Each surface answers that refusal as what it
// is, places nothing and keeps the checkout ready. UCP releases its key:
// the same key runs again. ACP's key keeps the refusal (§6.3), which a
// retry with it is answered again without reaching the store. Once Django
// lets the order through (the earlier one was canceled unpaid), a complete
// places this checkout's order: UCP's with the same key, ACP's with a new
// one.
func TestAnUnpaidCartRefusalIsAnsweredAsWhatItIs(t *testing.T) {
	stack := startUCPGateway(t)
	for _, surface := range completionSurfaces(t, stack) {
		t.Run(surface.name, func(t *testing.T) {
			ctx := context.Background()
			id := surface.ready(t)
			stack.faults.orderUnpaid.Store(true)
			t.Cleanup(func() { stack.faults.orderUnpaid.Store(false) })
			creates := stack.faults.orderCreates.Load()
			key := uuid.NewString()

			reached := int32(0)
			for attempt := 1; attempt <= 2; attempt++ {
				a := surface.answer(ctx, id, key, nil)

				assert.Empty(t, a.status, "answered as placed")
				assertUnpaidRefusal(t, surface.name, a)
				assert.Equal(t, bookkeeping{
					Session: checkout.StatusReadyForComplete,
					Key:     refusalKey(surface.name),
				}, stack.bookkeeping(t, id, surface.keyName(id, key)))
				if attempt == 1 || surface.name == "ucp" {
					reached++
				}
				assert.Equal(t, creates+reached,
					stack.faults.orderCreates.Load(),
					"attempt %d with the key", attempt)
			}

			stack.faults.orderUnpaid.Store(false)
			next := key
			if surface.name == "acp" {
				next = uuid.NewString()
			}
			assert.Equal(t, "completed", surface.complete(ctx, id, next, nil))
		})
	}

	// The store's code is counted as a refusal, as every code Django
	// gives is.
	assert.Contains(t, scrapeToolCalls(t, stack),
		`outcome="refused",server="mcp",tool="complete_checkout"`)
}

// assertUnpaidRefusal checks a completion refused because the cart's
// earlier order awaits its payment: ACP's own code in the spec's Error,
// UCP Django's code and sentence with what to do. Neither says to correct
// the arguments, and neither names an order.
func assertUnpaidRefusal(t *testing.T, surface string, a surfaceAnswer) {
	t.Helper()
	if surface == "acp" {
		require.Equal(t, "400 cart_has_unpaid_order", a.refusal)
		schema, err := acpErrorSchema()
		require.NoError(t, err)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(a.errorBody))
		require.NoError(t, err, "%s", a.errorBody)
		assert.NoError(t, schema.Validate(doc), "%s", a.errorBody)
		var body map[string]any
		require.NoError(t, json.Unmarshal(a.errorBody, &body))
		assert.Equal(t, "invalid_request", body["type"])
		message, _ := body["message"].(string)
		assert.Contains(t, message, "already awaits its payment")
		assert.Contains(t, message, "Do not create another session")
		assert.NotContains(t, message, djangoUnpaidSentence,
			"the gateway's own sentence, not Django's")
		assert.NotContains(t, message, fixtureOrderUUID)
		return
	}
	assert.Contains(t, a.refusal, `"code":"cart_has_unpaid_order"`)
	assert.Contains(t, a.refusal, djangoUnpaidSentence)
	assert.Contains(t, a.refusal, "so this checkout placed no order")
	assert.Contains(t, a.refusal, "do not start another checkout")
	assert.Contains(t, a.refusal, "this checkout can be completed")
	assert.NotContains(t, a.refusal, "correct the arguments")
	assert.NotContains(t, a.refusal, fixtureOrderUUID)
}

// scrapeToolCalls is the gateway's gateway_tool_calls_total lines.
func scrapeToolCalls(t *testing.T, stack ucpStack) string {
	t.Helper()
	resp, err := http.Get(stack.gw.URL + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var lines []string
	for line := range strings.Lines(string(raw)) {
		if strings.HasPrefix(line, "gateway_tool_calls_total{") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "")
}

// A create naming a cart Django no longer serves (a guest cart an order
// emptied or a sign-in merged is removed) is told an order may exist,
// whether the render or the coupon call finds the cart gone, and leaves
// nothing behind. A Django outage on the same read is still an outage.
func TestACreateOnACartDjangoNoLongerServes(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	// The store takes discount codes, so a create that sends one reaches
	// the coupon call; set before the store is first resolved.
	stack.faults.promotions.Store(true)
	p := newPlatform(t, stack)
	nothingLeft := func(t *testing.T, cart string) {
		t.Helper()
		assert.Zero(t, stack.sessionCount(t),
			"a checkout nobody was given was kept")
		assert.Empty(t, heldBy(t, stack.rdb, cart))
		keys, err := stack.rdb.Keys(ctx, cartRecordPrefix+"*").Result()
		require.NoError(t, err)
		assert.Empty(t, keys)
	}

	t.Run("a cart that is gone", func(t *testing.T) {
		// The fake never made it: asked for it, it serves another cart,
		// as Django serves a new guest cart for one it removed.
		cart := uuid.NewString()

		a := p.create(ctx, cart, nil)

		assert.Empty(t, a.id)
		assert.Contains(t, a.refusal, `"code":"not_found"`)
		assert.Contains(t, a.refusal, "that cart no longer exists: the "+
			"shopper may have checked it out on the store's site, so an "+
			"order may exist")
		assert.Contains(t, a.refusal, "No checkout was created")
		assert.NotContains(t, a.refusal, "retry")
		nothingLeft(t, cart)
	})

	// The coupon call is the first to find the cart gone when the create
	// sends a discount code.
	t.Run("a cart that is gone, with a discount code", func(t *testing.T) {
		cart := uuid.NewString()

		a := p.create(ctx, cart, map[string]any{
			"discounts": map[string]any{"codes": []string{"SAVE10"}},
		})

		assert.Empty(t, a.id)
		assert.Contains(t, a.refusal, `"code":"not_found"`)
		assert.Contains(t, a.refusal, "so an order may exist")
		assert.Contains(t, a.refusal, "No checkout was created")
		assert.NotContains(t, a.refusal, "start a new checkout")
		nothingLeft(t, cart)
	})

	t.Run("a cart Django cannot read now", func(t *testing.T) {
		cart := stack.faults.carts.mint()
		stack.faults.namedCartReadsFailFrom.Store(
			stack.faults.namedCartReads.Load() + 1)
		t.Cleanup(func() { stack.faults.namedCartReadsFailFrom.Store(0) })

		a := p.create(ctx, cart, nil)

		assert.Empty(t, a.id)
		assert.Contains(t, a.refusal, `"code":"unavailable"`)
		assert.NotContains(t, a.refusal, "no longer exists")
		nothingLeft(t, cart)
	})
}

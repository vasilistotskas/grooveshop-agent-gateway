//go:build integration

package integration

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A checkout's order is placed at the total the checkout showed. A
// completion sends it, and Django refuses another (the total changed
// since). That refusal places nothing and leaves the checkout ready, and
// the answer gives the new total: UCP shows the checkout at it, and ACP
// tells the platform to read it. The next completion places the order
// at the total the checkout shows then.
func TestAnOrderIsPlacedAtTheTotalItsCheckoutShowed(t *testing.T) {
	stack := startUCPGateway(t)
	for _, surface := range completionSurfaces(t, stack) {
		t.Run(surface.name, func(t *testing.T) {
			ctx := t.Context()
			id := surface.ready(t)
			placed := stack.faults.ordersPlaced.Load()

			stack.faults.totalChanged.Store(true)
			refused := surface.answer(ctx, id, uuid.NewString(), nil)
			stack.faults.totalChanged.Store(false)

			assert.Equal(t, placed, stack.faults.ordersPlaced.Load(),
				"nothing is placed at a total the checkout did not show")
			switch surface.name {
			case "ucp":
				assert.Equal(t, "incomplete", refused.status)
				assert.Contains(t, refused.text, "no order was placed")
			case "acp":
				assert.Equal(t, "400 total_changed", refused.refusal)
			}
			read := surface.read(ctx, id)
			assert.Contains(t,
				[]string{"ready_for_complete", "ready_for_payment"},
				read.status, "the checkout is ready again")

			assert.Equal(t, "completed",
				surface.complete(ctx, id, uuid.NewString(), nil))
			order := stack.faults.lastOrder.Load()
			require.NotNil(t, order)
			assert.Equal(t, map[string]any{
				"amount":   float64(fixtureShownTotal.Amount),
				"currency": fixtureShownTotal.Currency,
			}, (*order)["expectedTotal"],
				"the order is placed at the total the checkout showed")
		})
	}
}

// A total that cannot be read is unavailability, never a total without
// the rows it could not price: neither surface renders the checkout then.
func TestAQuoteThatCannotBeReadIsUnavailable(t *testing.T) {
	stack := startUCPGateway(t)
	for _, surface := range completionSurfaces(t, stack) {
		t.Run(surface.name, func(t *testing.T) {
			ctx := t.Context()
			id := surface.ready(t)

			stack.faults.quoteDown.Store(true)
			defer stack.faults.quoteDown.Store(false)
			for name, answer := range map[string]surfaceAnswer{
				"read":   surface.read(ctx, id),
				"update": surface.update(ctx, id),
			} {
				assert.Empty(t, answer.status, name)
				switch surface.name {
				case "acp":
					assert.Equal(t, "503 unavailable", answer.refusal, name)
				case "ucp":
					assert.Contains(t, answer.refusal, "unavailable", name)
				}
			}
		})
	}
}

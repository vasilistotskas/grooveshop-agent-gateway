//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// A checkout's order is placed at the total the checkout showed. A
// completion sends it, and Django refuses another (the total changed
// since). That refusal places nothing and leaves the checkout ready, and
// the answer shows no total: it tells the platform to read the
// checkout, which shows and stores the total now. The next completion places the order
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
			assert.Empty(t, refused.status, "no total is shown")
			switch surface.name {
			case "ucp":
				assert.Contains(t, refused.refusal, "total_changed")
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

// A price that changes after the checkout showed its total (the store's
// cash-on-delivery fee now applies) is not charged unseen: the
// completion is refused and places nothing, a read shows and stores the
// new total, and the next completion places the order at it.
func TestAChangedPriceIsChargedOnlyOnceShown(t *testing.T) {
	stack := startUCPGateway(t)
	for _, surface := range completionSurfaces(t, stack) {
		t.Run(surface.name, func(t *testing.T) {
			ctx := t.Context()
			id := surface.ready(t)
			placed := stack.faults.ordersPlaced.Load()

			stack.faults.codFee.Store(true)
			defer stack.faults.codFee.Store(false)
			key := uuid.NewString()
			refused := surface.answer(ctx, id, key, nil)
			assert.Equal(t, placed, stack.faults.ordersPlaced.Load(),
				"nothing is charged at a total the buyer was not shown")
			if surface.name == "acp" {
				assert.Equal(t, "400 total_changed", refused.refusal)
			}
			assert.Empty(t, refused.status, "the answer shows no total")
			// A retry under the same key, its answer lost, sends the
			// total stored before: still refused, nothing placed.
			again := surface.answer(ctx, id, key, nil)
			assert.Empty(t, again.status)
			assert.Equal(t, placed, stack.faults.ordersPlaced.Load())
			surface.read(ctx, id)

			assert.Equal(t, "completed",
				surface.complete(ctx, id, uuid.NewString(), nil))
			order := stack.faults.lastOrder.Load()
			require.NotNil(t, order)
			assert.Equal(t, map[string]any{
				"amount":   float64(fixtureShownTotal.Amount + 450),
				"currency": fixtureShownTotal.Currency,
			}, (*order)["expectedTotal"], "the total the read showed")
		})
	}
}

// A choice the store refuses for the cart now (cash on delivery is not
// offered for the checkout's carrier) leaves its total without that row:
// the checkout renders not ready, and a completion places nothing.
func TestARefusedChoiceIsNotCompleted(t *testing.T) {
	stack := startUCPGateway(t)
	for _, surface := range completionSurfaces(t, stack) {
		t.Run(surface.name, func(t *testing.T) {
			ctx := t.Context()
			id := surface.ready(t)
			placed := stack.faults.ordersPlaced.Load()
			sent := stack.faults.orderCreates.Load()

			stack.faults.codNotDelivered.Store(true)
			defer stack.faults.codNotDelivered.Store(false)
			read := surface.read(ctx, id)
			refused := surface.answer(ctx, id, uuid.NewString(), nil)

			assert.Contains(t,
				[]string{"incomplete", "not_ready_for_payment"}, read.status)
			assert.Empty(t, refused.status, "no order, no checkout placed")
			assert.NotEmpty(t, refused.refusal)
			assert.Equal(t, placed, stack.faults.ordersPlaced.Load())
			assert.Equal(t, sent, stack.faults.orderCreates.Load(),
				"the refused choice never reaches the store's order call")
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

// A checkout's order is made of the cart it showed (#142). A cart that
// changed at the same total since (a line swapped for another product at
// its price, as an update whose lock lapsed can write under a running
// completion) is refused by Django: the completion places nothing and
// says the cart changed, the checkout is ready again, and a read shows
// the cart now. The next completion places that cart, at the same total.
func TestACartChangedAtTheSameTotalIsNotOrdered(t *testing.T) {
	stack := startUCPGateway(t)
	for _, surface := range completionSurfaces(t, stack) {
		t.Run(surface.name, func(t *testing.T) {
			ctx := t.Context()
			id := surface.ready(t)
			placed := stack.faults.ordersPlaced.Load()

			stack.faults.lineSwapped.Store(true)
			defer stack.faults.lineSwapped.Store(false)
			key := uuid.NewString()
			refused := surface.answer(ctx, id, key, nil)

			assert.Equal(t, placed, stack.faults.ordersPlaced.Load(),
				"nothing is placed from a cart the checkout did not show")
			assert.Empty(t, refused.status, "no checkout is shown")
			switch surface.name {
			case "ucp":
				assert.Contains(t, refused.refusal, "cart_changed")
			case "acp":
				assert.Equal(t, "400 cart_changed", refused.refusal)
			}
			// A retry under the same key, its answer lost, sends the cart
			// stored before: still refused, nothing placed.
			again := surface.answer(ctx, id, key, nil)
			assert.Empty(t, again.status)
			assert.Equal(t, placed, stack.faults.ordersPlaced.Load())

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
			}, (*order)["expectedTotal"], "at the same total")
		})
	}
}

// A pickup order is made of the point its checkout's quote priced: the
// quote names the locker the order is sent to, so the cart digest the
// checkout showed is the order's. An order sent to another point than
// the one shown is refused, and nothing is placed.
func TestAPickupOrderIsSentToThePointItsCheckoutShowed(t *testing.T) {
	f := startFlow(t, 5*time.Second)
	pickup := func(t *testing.T, shownAt, orderedAt string) *checkout.Session {
		t.Helper()
		s := checkout.NewSession(f.tenant.SchemaName, f.tenant.Domain,
			checkout.ProtocolUCP, f.faults.carts.mint())
		s.Buyer = checkout.Buyer{
			FirstName: "Μαρία", LastName: "Παπαδοπούλου",
			Email: "maria@example.test", Phone: "+306912345678",
		}
		s.Fulfillment = checkout.Fulfillment{
			Kind:         checkout.FulfillmentPickupPoint,
			ProviderCode: django.ShippingProviderBoxNow,
			CountryCode:  "GR", City: "Αθήνα", Zipcode: "10431",
			Street: "Πανεπιστημίου", StreetNumber: "12", RegionID: "GR-14",
			BoxnowLockerID: shownAt,
		}
		s.PayWayID = 1
		s.Recompute()
		require.Equal(t, checkout.StatusReadyForComplete, s.Status)
		p, _, err := checkout.ComputePricing(t.Context(), f.dj, f.tenant, s)
		require.NoError(t, err)
		require.True(t, s.Show(p))
		s.Fulfillment.BoxnowLockerID = orderedAt
		require.NoError(t, f.store.Create(t.Context(), s,
			checkout.CartHolder{}))
		return s
	}

	t.Run("the point shown", func(t *testing.T) {
		s := pickup(t, "4", "4")
		_, err := f.flow.Complete(t.Context(), f.tenant, s)
		require.NoError(t, err)
		order := f.faults.lastOrder.Load()
		require.NotNil(t, order)
		assert.Equal(t, "4", (*order)["boxnowLockerId"])
		assert.Equal(t, s.Shown.CartDigest, (*order)["expectedCartDigest"])
		assert.Equal(t, checkout.StatusCompleted, f.stored(t, s).Status)
	})

	t.Run("another point", func(t *testing.T) {
		placed := f.faults.ordersPlaced.Load()
		s := pickup(t, "4", "5")
		_, err := f.flow.Complete(t.Context(), f.tenant, s)
		require.ErrorIs(t, err, django.ErrCartChanged)
		assert.Equal(t, placed, f.faults.ordersPlaced.Load())
		assert.Equal(t, checkout.StatusReadyForComplete,
			f.stored(t, s).Status)
	})
}

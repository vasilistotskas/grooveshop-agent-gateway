//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// A call that changes a session holds a snapshot of it, and the lock
// meant to keep the snapshot current can lapse. Whatever the call then
// answers from the snapshot, or from what Django said about the cart, is
// checked against the stored session first: if another request stored
// it meanwhile, the call is answered with what is stored.
//
// beforeScriptOn runs act once, ahead of the next script that names
// key: between a call's load of its session and that step of it.
func (stack ucpStack) beforeScriptOn(t *testing.T, key string, act func()) {
	t.Helper()
	var armed atomic.Bool
	armed.Store(true)
	stack.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if savesKey(cmd, key) && armed.CompareAndSwap(true, false) {
			act()
		}
		return nil
	}))
	t.Cleanup(func() { armed.Store(false) })
}

// An update's cart LINE write after another complete's order: the line
// went with the cart (Django answers a change 404), or the order took
// the stock (400). Neither says the cart is unreachable, and each used
// to be answered as the cart's refusal, "start a new one" included.
func TestAnUpdateWhoseLineWriteMeetsAnotherCompletesOrder(t *testing.T) {
	cases := []struct {
		name string
		// placed is how the winner completes, and what it leaves.
		submit map[string]any
		status string
		// refuse makes the fake answer the held line write as Django
		// does once that order exists.
		refuse func(*checkoutFaults)
	}{
		{"cash on delivery: the line is gone", nil, "completed",
			func(f *checkoutFaults) { f.cartDeletedOnOrder.Store(true) }},
		{"hosted payment: the order took the stock", changeBy("ucp"),
			"requires_escalation",
			func(f *checkoutFaults) { f.lineShort.Store(true) }},
	}
	for _, tc := range cases {
		for _, name := range []string{"acp", "ucp"} {
			t.Run(tc.name+", over "+name, func(t *testing.T) {
				ctx := context.Background()
				stack := startUCPGateway(t)
				surfaces := surfacesByName(t, stack)
				id := surfaces[name].ready(t)
				// The update has read the cart and changes its line.
				held := stack.hold(t, "PUT /api/v1/cart/item/410", 1)[0]
				updated := make(chan surfaceAnswer, 1)
				go func() { updated <- surfaces[name].updateLines(ctx, id) }()
				await(t, held, "the update's line write")
				stack.lapse(t, lockRedisKey(id))
				require.Equal(t, tc.status, surfaces["ucp"].complete(
					ctx, id, uuid.NewString(), tc.submit))
				placed := stack.storedBytes(t, id)
				tc.refuse(stack.faults)

				held.open()
				answer := <-updated
				assert.Empty(t, answer.status)
				assert.Contains(t, answer.refusal, orderPlaced[name])
				assert.NotContains(t, answer.refusal, "create_checkout")
				assert.NotContains(t, answer.refusal, "not_found")
				assert.NotContains(t, answer.refusal, "stock")
				assert.Equal(t, placed, stack.storedBytes(t, id))
				assert.EqualValues(t, 1, stack.faults.orderCreates.Load())
			})
		}
	}
}

// An update asks the store what it needs before its first cart write,
// and the lock can lapse while it waits. If another request stored the
// checkout meanwhile, the update writes nothing to the cart: a cart
// whose order is placed is not changed by a call that is then refused.
func TestAnUpdatePastItsLockWritesNoCartOfACheckoutStoredSince(
	t *testing.T,
) {
	for _, name := range []string{"acp", "ucp"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			stack := startUCPGateway(t)
			surfaces := surfacesByName(t, stack)
			surface := surfaces[name]
			id := surface.ready(t)
			held := stack.hold(t, surface.asked, 1)[0]
			updated := make(chan surfaceAnswer, 1)
			go func() { updated <- surface.updateAsking(ctx, id) }()
			await(t, held, "what the update asks the store")
			stack.lapse(t, lockRedisKey(id))
			// Paid on the store's page: the order keeps its cart.
			require.Equal(t, "requires_escalation", surfaces["ucp"].complete(
				ctx, id, uuid.NewString(), changeBy("ucp")))
			placed := stack.storedBytes(t, id)
			writes := stack.faults.cartWrites.Load()

			held.open()
			answer := <-updated
			assert.Empty(t, answer.status)
			assert.Contains(t, answer.refusal, orderPlaced[name])
			assert.Equal(t, writes, stack.faults.cartWrites.Load(),
				"the update wrote to the cart of a placed order")
			assert.Equal(t, placed, stack.storedBytes(t, id))
		})
	}

	// Nobody stored it meanwhile: the update goes through.
	t.Run("a checkout nobody touched", func(t *testing.T) {
		ctx := context.Background()
		stack := startUCPGateway(t)
		for name, surface := range surfacesByName(t, stack) {
			id := surface.ready(t)
			writes := stack.faults.cartWrites.Load()
			answer := surface.updateAsking(ctx, id)
			assert.NotEmpty(t, answer.status, "%s: %s", name, answer.refusal)
			assert.Greater(t, stack.faults.cartWrites.Load(), writes, name)
		}
	})
}

// A complete of a checkout that already placed its order renders it,
// from what the call loaded. If the payment landed meanwhile, the call
// answers the stored checkout: completed, not still awaiting a payment
// that was made.
func TestARenderFromAStaleSnapshotAnswersTheStoredCheckout(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surfaces := surfacesByName(t, stack)
	for _, name := range []string{"acp", "ucp"} {
		t.Run(name, func(t *testing.T) {
			id := awaitingPayment(t, surfaces["ucp"])
			stack.onSessionRead(t, id, exitRead, func() error {
				status, err := stack.orderEvent("COMPLETED")
				if err == nil && status != http.StatusNoContent {
					err = fmt.Errorf("the payment event answered %d", status)
				}
				return err
			})

			answer := surfaces[name].answer(ctx, id, uuid.NewString(), nil)

			assert.Equal(t, "completed", answer.status, answer.refusal)
			assert.Equal(t, checkout.StatusCompleted,
				stack.storedSession(t, id).Status)
		})
	}
}

// The save that marks a session in progress is what guards the order.
// When Redis refuses it no order is sent, and the caller is told the
// checkout records are unavailable: not Django's wording, and not that
// anything is in progress. Its key is free, so the same call, sent
// again, places the order.
func TestACompleteThatCannotMarkItsSessionSaysRecordsAreUnavailable(
	t *testing.T,
) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	want := map[string]string{
		"acp": "503 unavailable",
		"ucp": "checkout records are temporarily unavailable",
	}
	for name, surface := range surfacesByName(t, stack) {
		t.Run(name, func(t *testing.T) {
			id := surface.ready(t)
			key := uuid.NewString()
			creates := stack.faults.orderCreates.Load()
			var failed atomic.Bool
			stack.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
				if savesKey(cmd, sessionRedisKey(id)) &&
					failed.CompareAndSwap(false, true) {
					return errRedisDown
				}
				return nil
			}))

			answer := surface.answer(ctx, id, key, nil)

			assert.Empty(t, answer.status)
			assert.Contains(t, answer.refusal, want[name])
			if name == "acp" {
				assert.Contains(t, string(answer.errorBody),
					"Checkout is temporarily unavailable.")
			}
			assert.Equal(t, creates, stack.faults.orderCreates.Load())
			assert.Equal(t, checkout.StatusReadyForComplete,
				stack.storedSession(t, id).Status)
			require.Equal(t, "completed",
				surface.complete(ctx, id, key, nil))
		})
	}
}

// A complete that finds its idempotency key spent is the retry of an
// attempt that finished. What that attempt made of the session is what
// is stored, not what this call loaded before it: answered from its own
// snapshot it would say "ready, call complete to place the order" for a
// checkout that has one.
func TestACompleteOnASpentKeyAnswersTheStoredCheckout(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surfaces := surfacesByName(t, stack)

	// UCP looks the payment up before it claims its key: a slow store
	// holds the first call there, past its lock.
	t.Run("ucp", func(t *testing.T) {
		surface := surfaces["ucp"]
		id := surface.ready(t)
		key := uuid.NewString()
		creates := stack.faults.orderCreates.Load()
		held := stack.hold(t, "GET /api/v1/pay_way", 1)[0]
		first := make(chan surfaceAnswer, 1)
		go func() {
			first <- surface.answer(ctx, id, key,
				map[string]any{"payment": codPayment()})
		}()
		await(t, held, "the first complete's payment lookup")
		stack.lapse(t, lockRedisKey(id))
		require.Equal(t, "completed", surface.complete(ctx, id, key, nil))

		held.open()
		assertStored(t, <-first, "completed")
		assert.Equal(t, creates+1, stack.faults.orderCreates.Load())
	})

	// ACP claims its key right after loading: the retry runs between
	// the two.
	t.Run("acp", func(t *testing.T) {
		surface := surfaces["acp"]
		id := surface.ready(t)
		key := uuid.NewString()
		creates := stack.faults.orderCreates.Load()
		retried := make(chan surfaceAnswer, 1)
		stack.beforeScriptOn(t, surface.keyName(id, key), func() {
			_ = stack.rdb.Del(ctx, lockRedisKey(id)).Err()
			retried <- surface.answer(ctx, id, key, nil)
		})

		first := surface.answer(ctx, id, key, nil)

		require.Equal(t, "completed", (<-retried).status)
		assertStored(t, first, "completed")
		assert.Equal(t, creates+1, stack.faults.orderCreates.Load())
	})
}

// A refusal decided from the snapshot (the session is not ready, the
// payment is missing or unsupported) describes the session as the call
// loaded it. If another complete placed the order meanwhile, the call is
// answered with the placed checkout, not told to fix a checkout that
// has an order.
func TestARefusalFromAStaleSnapshotAnswersTheStoredCheckout(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surfaces := surfacesByName(t, stack)
	// meanwhile places the order just before the call under test reads
	// its session again.
	meanwhile := func(t *testing.T, id string) {
		t.Helper()
		stack.onSessionRead(t, id, exitRead, func() error {
			if err := stack.rdb.Del(ctx, lockRedisKey(id)).Err(); err != nil {
				return err
			}
			if surfaces["ucp"].complete(ctx, id, uuid.NewString(),
				map[string]any{"payment": codPayment()}) != "completed" {
				return errors.New("the other complete did not place")
			}
			return nil
		})
	}

	t.Run("acp: payment_data is missing", func(t *testing.T) {
		id := surfaces["acp"].ready(t)
		meanwhile(t, id)
		answer := acpAnswer(ctx, stack.gw.URL+"/acp/checkout_sessions/"+id+
			"/complete", uuid.NewString(), map[string]any{})
		assertStored(t, answer, "completed")
	})
	t.Run("acp: a delegated payment", func(t *testing.T) {
		id := surfaces["acp"].ready(t)
		meanwhile(t, id)
		answer := surfaces["acp"].answer(ctx, id, uuid.NewString(),
			map[string]any{"payment_data": map[string]any{
				"handler_id": "com.example.cards",
			}})
		assertStored(t, answer, "completed")
	})
	t.Run("ucp: an instrument the store does not offer", func(t *testing.T) {
		id := surfaces["ucp"].ready(t)
		meanwhile(t, id)
		answer := surfaces["ucp"].answer(ctx, id, uuid.NewString(),
			map[string]any{"payment": map[string]any{
				"instruments": []any{map[string]any{
					"handler_id": "com.example.elsewhere",
					"type":       "card", "selected": true,
				}},
			}})
		assertStored(t, answer, "completed")
	})
	// Without the other complete, each refusal stands.
	t.Run("the refusals of a session nobody touched", func(t *testing.T) {
		id := surfaces["acp"].ready(t)
		assert.Equal(t, "400 missing", acpAnswer(ctx, stack.gw.URL+
			"/acp/checkout_sessions/"+id+"/complete", uuid.NewString(),
			map[string]any{}).refusal)
		assert.Equal(t, checkout.StatusReadyForComplete,
			stack.storedSession(t, id).Status)
	})
}

// A completion places its order, and its last save fails: the session
// stays in progress and does not name the order. The first caller is
// told the order. So is everyone after it: the order was recorded beside
// the session when Django returned it. And any event for that order,
// which a cash-on-delivery order's first event is, records it on the
// session; the payment event then completes it.
func TestAnOrderItsSessionNeverSavedIsStillKnown(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surfaces := surfacesByName(t, stack)
	redisDown := errors.New("redis is down")
	// placedUnsaved returns a checkout whose order is placed and whose
	// last save failed.
	placedUnsaved := func(t *testing.T) string {
		t.Helper()
		id := surfaces["ucp"].ready(t)
		var linked, failed atomic.Bool
		stack.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
			switch {
			case setsKey(cmd, ":orderlink:"):
				linked.Store(true)
			case linked.Load() && savesKey(cmd, sessionRedisKey(id)) &&
				failed.CompareAndSwap(false, true):
				return redisDown
			}
			return nil
		}))
		first := surfaces["ucp"].answer(ctx, id, uuid.NewString(), nil)
		require.Contains(t, first.refusal, "the order was placed")
		require.Contains(t, first.refusal, fixtureOrderUUID)
		stored := stack.storedSession(t, id)
		require.Equal(t, checkout.StatusCompleteInProgress, stored.Status)
		require.Empty(t, stored.OrderUUID)
		return id
	}

	t.Run("later calls are told the order", func(t *testing.T) {
		id := placedUnsaved(t)
		for what, answer := range map[string]surfaceAnswer{
			"update":   surfaces["acp"].update(ctx, id),
			"complete": surfaces["acp"].answer(ctx, id, uuid.NewString(), nil),
			"cancel":   surfaces["acp"].abandon(ctx, id),
		} {
			assert.Equal(t, "500 order_placed", answer.refusal, what)
		}
		for what, answer := range map[string]surfaceAnswer{
			"update":   surfaces["ucp"].update(ctx, id),
			"complete": surfaces["ucp"].answer(ctx, id, uuid.NewString(), nil),
			"cancel":   surfaces["ucp"].abandon(ctx, id),
		} {
			assert.Contains(t, answer.refusal, "the order was placed", what)
			assert.Contains(t, answer.refusal, fixtureOrderUUID, what)
		}
		// A read that renders says it too.
		read := surfaces["ucp"].read(ctx, id)
		assert.Contains(t, read.text, fixtureOrderUUID)
	})

	t.Run("any event records the order, the payment completes it",
		func(t *testing.T) {
			id := placedUnsaved(t)
			require.Equal(t, http.StatusNoContent,
				stack.pushOrderEvent(t, "PENDING"))
			named := stack.storedSession(t, id)
			assert.Equal(t, checkout.StatusCompleteInProgress, named.Status)
			assert.Equal(t, fixtureOrderUUID, named.OrderUUID)
			assert.EqualValues(t, 684, named.OrderID)
			assert.False(t, named.PlacedAt.IsZero())

			require.Equal(t, http.StatusNoContent,
				stack.pushOrderEvent(t, "COMPLETED"))
			assert.Equal(t, checkout.StatusCompleted,
				stack.storedSession(t, id).Status)
		})

	// Without the record beside the session the gateway does not know
	// the order's id: the session is left as it is, never completed
	// without its order, and the event is not asked for again.
	t.Run("an event without the placed order's record", func(t *testing.T) {
		id := placedUnsaved(t)
		stack.lapse(t, sessionRedisKey(id)+":order")
		before := stack.storedBytes(t, id)

		assert.Equal(t, http.StatusNoContent,
			stack.pushOrderEvent(t, "COMPLETED"))

		assert.Equal(t, before, stack.storedBytes(t, id))
		assert.NotEmpty(t,
			stack.logs.with("no placed order", "session="+id))
	})
}

// An order event takes no session lock: its save is a compare-and-set,
// and it is answered at once. It can land while the completion that
// placed its order is still finishing (the order exists, its payment
// page is being opened). The completion's last save then finds the
// session stored by the event, which is the only other writer of a
// session in progress, and stores its own outcome over it: the order is
// not reported as unsaved, and what the event said (paid) is kept.
func TestAnOrderEventDuringItsCompletionCostsNeitherTheirResult(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surface := surfacesByName(t, stack)["ucp"]
	id := surface.ready(t)
	held := stack.hold(t,
		"POST /api/v1/order/684/create_checkout_session", 1)[0]
	completed := make(chan surfaceAnswer, 1)
	go func() {
		completed <- surface.answer(ctx, id, uuid.NewString(), changeBy("ucp"))
	}()
	await(t, held, "the completion's payment page")

	require.Equal(t, http.StatusNoContent,
		stack.pushOrderEvent(t, "COMPLETED"))
	require.Equal(t, checkout.StatusCompleted,
		stack.storedSession(t, id).Status)

	held.open()
	done := <-completed
	assert.Equal(t, "completed", done.status, done.refusal)
	stored := stack.storedSession(t, id)
	assert.Equal(t, checkout.StatusCompleted, stored.Status)
	assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
	assert.Equal(t, vivaCheckoutURL, stored.PaymentURL)
	assert.EqualValues(t, 1, stack.faults.orderCreates.Load())
}

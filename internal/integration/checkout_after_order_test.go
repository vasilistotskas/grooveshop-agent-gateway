//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// surfacesByName starts nothing: it names the stack's two surfaces.
func surfacesByName(t *testing.T, stack ucpStack) map[string]completionSurface {
	t.Helper()
	out := map[string]completionSurface{}
	for _, surface := range completionSurfaces(t, stack) {
		out[surface.name] = surface
	}
	return out
}

// pushOrderEvent is Django's push of an order's payment status, and the
// status the gateway answered: 204 is final for Django, 503 makes it
// push again.
func (stack ucpStack) pushOrderEvent(t *testing.T, paymentStatus string) int {
	t.Helper()
	status, err := stack.orderEvent(paymentStatus)
	require.NoError(t, err)
	return status
}

// orderEvent is pushOrderEvent for any goroutine: it asserts nothing.
func (stack ucpStack) orderEvent(paymentStatus string) (int, error) {
	event, err := json.Marshal(map[string]any{
		"schemaName": "demostore", "orderUuid": fixtureOrderUUID,
		"status": "PROCESSING", "paymentStatus": paymentStatus,
	})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost,
		stack.gw.URL+"/internal/events/order-status", bytes.NewReader(event))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", internalSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// savesKey reports whether cmd is the script that stores key.
func savesKey(cmd redis.Cmder, key string) bool {
	args := cmd.Args()
	return (cmd.Name() == "evalsha" || cmd.Name() == "eval") &&
		len(args) > 3 && args[3] == key
}

// storedMeanwhile makes another request store session id just before the
// next save of it reaches Redis: what a request finds when it outlived
// its lock, without a slow store to hold it there.
func (stack ucpStack) storedMeanwhile(t *testing.T, id string) {
	t.Helper()
	store := checkout.NewStore(stack.rdb, ucpUpstreamTimeout)
	var armed atomic.Bool
	armed.Store(true)
	stack.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		if !savesKey(cmd, sessionRedisKey(id)) ||
			!armed.CompareAndSwap(true, false) {
			return nil
		}
		ctx := context.Background()
		other, err := store.Load(ctx, "demostore", id)
		if err != nil {
			return err
		}
		return store.Save(ctx, other)
	}))
	t.Cleanup(func() { armed.Store(false) })
}

// awaitingPayment returns a checkout whose order is placed and awaits
// its hosted payment.
func awaitingPayment(
	t *testing.T, ucpSurface completionSurface,
) string {
	t.Helper()
	id := ucpSurface.ready(t)
	require.Equal(t, "requires_escalation", ucpSurface.complete(
		context.Background(), id, uuid.NewString(), changeBy("ucp")))
	return id
}

// A save that lost to another request wrote nothing, and its request
// must say so: answered as done, a cancel would be believed, a payment
// link lost, and a payment event never delivered again.
func TestALostSaveIsNeverAnsweredAsDone(t *testing.T) {
	stack := startUCPGateway(t)
	ctx := context.Background()
	surfaces := surfacesByName(t, stack)

	for name, surface := range surfaces {
		t.Run("a cancel over "+name, func(t *testing.T) {
			id := surface.ready(t)
			stack.storedMeanwhile(t, id)

			assertLost(t, name, surface.abandon(ctx, id))

			assert.Equal(t, checkout.StatusReadyForComplete,
				stack.storedSession(t, id).Status)
			// Sent again, it reads the session as it is and cancels it.
			assert.Equal(t, "canceled", surface.abandon(ctx, id).status)
		})
	}

	// An update's cart writes are made by the time its save is refused:
	// the agent is told so.
	for name, surface := range surfaces {
		t.Run("an update over "+name, func(t *testing.T) {
			id := surface.ready(t)
			stack.storedMeanwhile(t, id)

			assertLostUpdate(t, name, surface.update(ctx, id))

			assert.NotEqual(t, otherBuyerEmail,
				stack.storedSession(t, id).Buyer.Email)
		})
	}

	// The order exists and its payment page could not be opened. The
	// next complete opens one, and must not answer with a link it could
	// not store.
	t.Run("a payment link opened again", func(t *testing.T) {
		ucpSurface := surfaces["ucp"]
		stack.faults.paymentLink.Store(true)
		t.Cleanup(func() { stack.faults.paymentLink.Store(false) })
		id := ucpSurface.ready(t)
		first := ucpSurface.answer(ctx, id, uuid.NewString(), changeBy("ucp"))
		require.Contains(t, first.refusal, "is placed")
		stack.faults.paymentLink.Store(false)
		require.Empty(t, stack.storedSession(t, id).PaymentURL)
		stack.storedMeanwhile(t, id)

		// The stored checkout still has no link, and that is what the
		// call is answered with: the order, without a payment page.
		lost := ucpSurface.answer(ctx, id, uuid.NewString(), nil)
		assert.Equal(t, "requires_escalation", lost.status, lost.refusal)
		assert.NotEqual(t, vivaCheckoutURL, lost.continueURL,
			"answered with a payment link that was not stored")
		assert.Empty(t, stack.storedSession(t, id).PaymentURL)
		assert.Equal(t, "requires_escalation",
			ucpSurface.complete(ctx, id, uuid.NewString(), nil))
		assert.Equal(t, vivaCheckoutURL,
			stack.storedSession(t, id).PaymentURL)
	})

	// 204 is final for Django. A payment event whose save lost must be
	// pushed again, or the checkout awaits a payment that was made.
	t.Run("a payment event", func(t *testing.T) {
		id := awaitingPayment(t, surfaces["ucp"])
		stack.storedMeanwhile(t, id)

		assert.Equal(t, http.StatusServiceUnavailable,
			stack.pushOrderEvent(t, "COMPLETED"))
		assert.Equal(t, checkout.StatusRequiresEscalation,
			stack.storedSession(t, id).Status)

		assert.Equal(t, http.StatusNoContent,
			stack.pushOrderEvent(t, "COMPLETED"))
		assert.Equal(t, checkout.StatusCompleted,
			stack.storedSession(t, id).Status)
	})
}

// Once an order exists for a checkout, or may, neither surface cancels
// or changes the checkout: that would not cancel or change the order,
// and the buyer could still pay a checkout that reads canceled.
func TestACheckoutThatMayHaveAnOrderTakesNoChange(t *testing.T) {
	stack := startUCPGateway(t)
	ctx := context.Background()
	surfaces := surfacesByName(t, stack)
	states := []struct {
		name string
		// reach returns a checkout in the state.
		reach func(t *testing.T) string
		want  checkout.Status
		// acp is how ACP refuses a change of it.
		acp string
	}{
		{"an order the store did not confirm", func(t *testing.T) string {
			t.Helper()
			id := surfaces["ucp"].ready(t)
			stack.faults.orderCreate.Store(true)
			defer stack.faults.orderCreate.Store(false)
			require.Empty(t, surfaces["ucp"].complete(
				ctx, id, uuid.NewString(), nil))
			return id
		}, checkout.StatusCompleteInProgress, "409 idempotency_in_flight"},
		{"an order that awaits its payment", func(t *testing.T) string {
			t.Helper()
			return awaitingPayment(t, surfaces["ucp"])
		}, checkout.StatusRequiresEscalation, "405 conflict"},
		{"a placed order", func(t *testing.T) string {
			t.Helper()
			id := surfaces["ucp"].ready(t)
			require.Equal(t, "completed", surfaces["ucp"].complete(
				ctx, id, uuid.NewString(), nil))
			return id
		}, checkout.StatusCompleted, "405 conflict"},
	}
	for _, state := range states {
		for name, surface := range surfaces {
			t.Run(state.name+" over "+name, func(t *testing.T) {
				id := state.reach(t)
				require.Equal(t, state.want,
					stack.storedSession(t, id).Status)
				stored := stack.storedBytes(t, id)
				// Kept for a day, not the half hour of an open checkout:
				// while it is held, a caller is told what became of it.
				ttl, err := stack.rdb.PTTL(ctx, sessionRedisKey(id)).Result()
				require.NoError(t, err)
				assert.Greater(t, ttl, 23*time.Hour)

				for what, call := range map[string]func(
					context.Context, string,
				) surfaceAnswer{
					"cancel": surface.abandon, "update": surface.update,
				} {
					answer := call(ctx, id)
					assert.Empty(t, answer.status, what)
					if name == "acp" {
						assert.Equal(t, state.acp, answer.refusal, what)
					} else {
						assert.NotContains(t, answer.refusal,
							"create_checkout", what)
					}
				}
				assert.Equal(t, stored, stack.storedBytes(t, id))
			})
		}
	}

	// A placed order is named, so the agent can read it.
	t.Run("a refusal names the order", func(t *testing.T) {
		id := awaitingPayment(t, surfaces["ucp"])
		for _, answer := range []surfaceAnswer{
			surfaces["ucp"].abandon(ctx, id), surfaces["ucp"].update(ctx, id),
		} {
			assert.Contains(t, answer.refusal,
				"already placed order "+fixtureOrderUUID)
		}
	})
}

// An update that outlived its lock while a complete placed the order
// writes a cart that went with the order. It used to answer that the
// cart was gone and to start a new checkout, which would have ordered
// again. It reads the session again and answers what it is now.
func TestAnUpdatePastItsLockIsNotToldToStartOver(t *testing.T) {
	want := map[string]string{
		"acp": "405 conflict",
		"ucp": "already placed order " + fixtureOrderUUID,
	}
	for name, refusal := range want {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			stack := startUCPGateway(t)
			surface := surfacesByName(t, stack)[name]
			id := surface.ready(t)
			// The update's first cart call: reading the lines it holds.
			held := stack.hold(t, "GET /api/v1/cart", 1)[0]
			updated := make(chan surfaceAnswer, 1)
			go func() { updated <- surface.updateLines(ctx, id) }()
			await(t, held, "the update")
			stack.lapse(t, lockRedisKey(id))
			stack.faults.cartDeletedOnOrder.Store(true)
			require.Equal(t, "completed",
				surface.answer(ctx, id, uuid.NewString(), nil).status)
			placed := stack.storedBytes(t, id)

			held.open()
			answer := <-updated
			assert.Empty(t, answer.status)
			assert.Contains(t, answer.refusal, refusal)
			assert.NotContains(t, answer.refusal, "create_checkout")
			assert.NotContains(t, answer.refusal, "not_found")
			assert.Equal(t, placed, stack.storedBytes(t, id))
			assert.EqualValues(t, 1, stack.faults.orderCreates.Load())
		})
	}

	// The cart of a checkout that is still open really is gone: that
	// answer is unchanged.
	t.Run("a cart that is gone under an open checkout", func(t *testing.T) {
		ctx := context.Background()
		stack := startUCPGateway(t)
		surfaces := surfacesByName(t, stack)
		id := surfaces["ucp"].ready(t)
		// Another checkout's order takes the cart.
		other := surfaces["ucp"].ready(t)
		stack.faults.cartDeletedOnOrder.Store(true)
		require.Equal(t, "completed", surfaces["ucp"].complete(
			ctx, other, uuid.NewString(), nil))

		assert.Equal(t, "400 not_found",
			surfaces["acp"].updateLines(ctx, id).refusal)
		gone := surfaces["ucp"].updateLines(ctx, id).refusal
		assert.Contains(t, gone, "the checkout's cart no longer exists")
		assert.Contains(t, gone, "The checkout was not updated")
	})
}

// A checkout the gateway holds no session for may have expired unplaced,
// or been placed and lost its record (expiry, eviction, a session kept
// under another key by an older release), or never existed. The id does
// not say which, so the answer never says only "start a new one".
func TestAMissingCheckoutIsNotOnlyStartANewOne(t *testing.T) {
	stack := startUCPGateway(t)
	ctx := context.Background()
	surfaces := surfacesByName(t, stack)
	base := stack.gw.URL + "/acp/checkout_sessions"

	// A session an older release stored, under its key: this one never
	// reads it, so it is as missing as an id that never existed.
	old := surfaces["ucp"].ready(t)
	raw := stack.storedBytes(t, old)
	require.NoError(t, stack.rdb.Rename(ctx, sessionRedisKey(old),
		"ag:demostore:cs:"+old).Err())
	require.NotEmpty(t, raw)

	for name, id := range map[string]string{
		"an id that never existed":    uuid.NewString(),
		"a session under the old key": old,
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := acpCall(t, http.MethodGet, base+"/"+id, nil, "")
			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
			assert.Equal(t, "not_found", body["code"])
			assert.Contains(t, body["message"], "an order may exist")

			for what, answer := range map[string]surfaceAnswer{
				"complete": surfaces["ucp"].answer(
					ctx, id, uuid.NewString(), nil),
				"update": surfaces["ucp"].update(ctx, id),
				"cancel": surfaces["ucp"].abandon(ctx, id),
				"get": mcpAnswer(ctx, connectMCP(t, stack.gw.URL),
					"get_checkout", map[string]any{
						"meta": map[string]any{"ucp-agent": map[string]any{
							"profile": servePlatformProfile(t,
								"http://127.0.0.1:1/ucp/orders", nil),
						}},
						"id": id,
					}),
			} {
				assert.Contains(t, answer.refusal,
					"an order may exist", what)
				assert.Contains(t, answer.refusal, "get_order", what)
				assert.Contains(t, answer.refusal,
					"do not start another checkout before checking", what)
			}
		})
	}
	// The old key's session was not touched either.
	kept, err := stack.rdb.Get(ctx, "ag:demostore:cs:"+old).Result()
	require.NoError(t, err)
	assert.Equal(t, raw, kept)
}

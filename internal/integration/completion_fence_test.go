//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// Where the gateway keeps a checkout session and its lock.
func sessionRedisKey(id string) string { return "ag:demostore:cs:v2:" + id }
func lockRedisKey(id string) string    { return sessionRedisKey(id) + ":lock" }

// gate is one request the fake Django holds: reached closes when the
// request arrives, and the fake answers it once open is called.
type gate struct {
	reached chan struct{}
	opened  chan struct{}
	open    func()
}

// hold makes the fake Django hold the next n requests for route
// ("POST /api/v1/order"), each at its own gate. Later ones pass.
func (stack ucpStack) hold(t *testing.T, route string, n int) []*gate {
	t.Helper()
	gates := make([]*gate, n)
	for i := range gates {
		opened := make(chan struct{})
		gates[i] = &gate{
			reached: make(chan struct{}), opened: opened,
			open: sync.OnceFunc(func() { close(opened) }),
		}
	}
	var seen atomic.Int32
	before := func(r *http.Request) {
		if r.Method+" "+r.URL.Path != route {
			return
		}
		i := int(seen.Add(1)) - 1
		if i >= n {
			return
		}
		close(gates[i].reached)
		<-gates[i].opened
	}
	stack.faults.before.Store(&before)
	t.Cleanup(func() {
		stack.faults.before.Store(nil)
		for _, g := range gates {
			g.open()
		}
	})
	return gates
}

// await waits for a held request to arrive. The fake holds its answer
// meanwhile, and must give it before the gateway gives up on it.
func await(t *testing.T, g *gate, what string) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(ucpUpstreamTimeout / 2):
		t.Fatalf("%s never reached the store", what)
	}
}

// lapse removes a key the way its expiry would, and requires that the
// request it belonged to was still holding it.
func (stack ucpStack) lapse(t *testing.T, key string) {
	t.Helper()
	n, err := stack.rdb.Del(context.Background(), key).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "%s was not held", key)
}

// The answer a call gets when another request changed its session
// meanwhile: ACP's 409, the changed-session text on UCP.
var sessionChanged = map[string]string{
	"acp": "409 idempotency_in_flight",
	"ucp": "changed by another request",
}

// assertLost checks that a call was told to come back, having changed
// nothing: on ACP with the spec's Error and a Retry-After.
func assertLost(t *testing.T, surface string, a surfaceAnswer) {
	t.Helper()
	assert.Empty(t, a.status, "the call was answered as if it had won")
	assert.Contains(t, a.refusal, sessionChanged[surface])
	if surface != "acp" || a.errorBody == nil {
		return
	}
	schema, err := acpErrorSchema()
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(a.errorBody))
	require.NoError(t, err, "%s", a.errorBody)
	assert.NoError(t, schema.Validate(doc), "%s", a.errorBody)
	after, err := strconv.Atoi(a.retryAfter)
	assert.NoError(t, err, "a 409 must carry Retry-After")
	assert.Positive(t, after)
}

// assertLostUpdate is assertLost for an update, whose cart writes were
// made before its save was refused: the agent is told that the rest of
// the update was not stored, and that its lines may be in the cart.
func assertLostUpdate(t *testing.T, surface string, a surfaceAnswer) {
	t.Helper()
	assertLost(t, surface, a)
	if surface == "ucp" {
		assert.Contains(t, a.refusal, "The checkout was not updated")
		assert.Contains(t, a.refusal, "may already be in the cart")
	}
}

// A completion looks the store's pay ways up and reserves stock before
// it places the order, and a slow store can hold it there past its
// session lock, and past its key's claim. A second complete then finds
// the session still ready. Exactly one of them places the order: the
// other sends nothing to the store and is told to come back.
func TestConcurrentCompletesPlaceOneOrder(t *testing.T) {
	stack := startUCPGateway(t)
	for _, surface := range completionSurfaces(t, stack) {
		for _, sameKey := range []bool{false, true} {
			name := "another key"
			if sameKey {
				name = "the same key, its claim lapsed"
			}
			t.Run(name+" over "+surface.name, func(t *testing.T) {
				ctx := context.Background()
				id := surface.ready(t)
				creates := stack.faults.orderCreates.Load()
				held := stack.hold(t, "POST /api/v1/cart/reserve-stock", 1)[0]

				firstKey := uuid.NewString()
				first := make(chan surfaceAnswer, 1)
				go func() { first <- surface.answer(ctx, id, firstKey, nil) }()
				await(t, held, "the first complete")

				stack.lapse(t, lockRedisKey(id))
				secondKey := uuid.NewString()
				if sameKey {
					stack.lapse(t, surface.keyName(id, firstKey))
					secondKey = firstKey
				}
				second := surface.answer(ctx, id, secondKey, nil)
				require.Equal(t, "completed", second.status,
					"the second complete: %s", second.refusal)
				require.Equal(t, creates+1, stack.faults.orderCreates.Load())

				held.open()
				assertLost(t, surface.name, <-first)
				assert.Equal(t, creates+1, stack.faults.orderCreates.Load(),
					"the complete that lost sent an order to the store")
				// The winner's order and its spent key stand.
				assert.Equal(t, bookkeeping{
					Session: checkout.StatusCompleted, Linked: true,
					Key: keySpent,
				}, stack.bookkeeping(t, id, surface.keyName(id, secondKey)))
				if !sameKey {
					assert.Equal(t, keyReleased, stack.bookkeeping(t, id,
						surface.keyName(id, firstKey)).Key,
						"the loser placed nothing, so its key is free")
				}
				assert.Equal(t, fixtureOrderUUID,
					stack.storedSession(t, id).OrderUUID)
			})
		}
	}
}

// For a pay way that is not settled online Django deletes the cart with
// the order, and refuses a second order from it. That refusal used to
// reset the loser's snapshot to ready and save it over the placed
// session, which the next complete would then have ordered again.
func TestALostCompleteLeavesThePlacedSessionAlone(t *testing.T) {
	for _, name := range []string{"acp", "ucp"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			stack := startUCPGateway(t)
			var surface completionSurface
			for _, s := range completionSurfaces(t, stack) {
				if s.name == name {
					surface = s
				}
			}
			id := surface.ready(t)
			stack.faults.cartDeletedOnOrder.Store(true)
			held := stack.hold(t, "POST /api/v1/cart/reserve-stock", 1)[0]

			first := make(chan surfaceAnswer, 1)
			go func() {
				first <- surface.answer(ctx, id, uuid.NewString(), nil)
			}()
			await(t, held, "the first complete")
			stack.lapse(t, lockRedisKey(id))
			second := surface.answer(ctx, id, uuid.NewString(), nil)
			require.Equal(t, "completed", second.status,
				"the second complete: %s", second.refusal)
			placed := stack.storedBytes(t, id)

			held.open()
			assertLost(t, name, <-first)
			assert.EqualValues(t, 1, stack.faults.orderCreates.Load())
			assert.Equal(t, placed, stack.storedBytes(t, id),
				"the placed session was written over")
		})
	}
}

// An update reads the store's pay ways before it saves, and can be held
// there past its lock while a complete places the order. Its snapshot is
// the session as it was before the order: saved, it would put a ready
// session over one that is being placed, or is placed.
func TestAStaleUpdateDoesNotOverwriteACompletion(t *testing.T) {
	stack := startUCPGateway(t)
	for _, surface := range completionSurfaces(t, stack) {
		t.Run("while the order is placed, over "+surface.name,
			func(t *testing.T) {
				ctx := context.Background()
				id := surface.ready(t)
				creates := stack.faults.orderCreates.Load()
				held := stack.hold(t, "GET /api/v1/pay_way", 1)[0]
				updated := make(chan surfaceAnswer, 1)
				go func() { updated <- surface.update(ctx, id) }()
				await(t, held, "the update")
				stack.lapse(t, lockRedisKey(id))

				ordering := make(chan struct{})
				answer := make(chan struct{})
				send := sync.OnceFunc(func() { close(answer) })
				defer send()
				orderHold := func(*http.Request) {
					close(ordering)
					<-answer
				}
				stack.faults.orderHold.Store(&orderHold)
				defer stack.faults.orderHold.Store(nil)
				completed := make(chan surfaceAnswer, 1)
				go func() {
					completed <- surface.answer(ctx, id, uuid.NewString(), nil)
				}()
				select {
				case <-ordering:
				case <-time.After(ucpUpstreamTimeout / 2):
					t.Fatal("the order was never sent to the store")
				}
				require.Equal(t, checkout.StatusCompleteInProgress,
					stack.storedSession(t, id).Status)

				held.open()
				assertLostUpdate(t, surface.name, <-updated)
				assert.Equal(t, checkout.StatusCompleteInProgress,
					stack.storedSession(t, id).Status,
					"the update wrote over a session being placed")

				send()
				assert.Equal(t, "completed", (<-completed).status)
				stored := stack.storedSession(t, id)
				assert.Equal(t, checkout.StatusCompleted, stored.Status)
				assert.Equal(t, fixtureOrderUUID, stored.OrderUUID)
				assert.NotEqual(t, otherBuyerEmail, stored.Buyer.Email)
				assert.Equal(t, creates+1, stack.faults.orderCreates.Load())
			})

		t.Run("after the order was placed, over "+surface.name,
			func(t *testing.T) {
				ctx := context.Background()
				id := surface.ready(t)
				creates := stack.faults.orderCreates.Load()
				held := stack.hold(t, "GET /api/v1/pay_way", 1)[0]
				updated := make(chan surfaceAnswer, 1)
				go func() { updated <- surface.update(ctx, id) }()
				await(t, held, "the update")
				stack.lapse(t, lockRedisKey(id))

				require.Equal(t, "completed",
					surface.answer(ctx, id, uuid.NewString(), nil).status)
				placed := stack.storedBytes(t, id)

				held.open()
				assertLostUpdate(t, surface.name, <-updated)
				assert.Equal(t, placed, stack.storedBytes(t, id),
					"the update wrote over a placed session")
				// A ready session here would be ordered a second time.
				assert.Equal(t, "completed",
					surface.answer(ctx, id, uuid.NewString(), nil).status)
				assert.Equal(t, creates+1, stack.faults.orderCreates.Load())
			})
	}
}

// An ACP request that runs past its key's claim no longer holds the key:
// the next request with that key does. What the first one then does
// about the key (release it when it fails, record it when it succeeds)
// must not touch the holder's claim or record.
func TestACPRequestPastItsClaimLeavesTheKeyToItsHolder(t *testing.T) {
	stack := startUCPGateway(t)
	base := stack.gw.URL + "/acp/checkout_sessions"
	ctx := context.Background()

	t.Run("failing, it does not release the holder's claim",
		func(t *testing.T) {
			id := acpReady(t, base)
			key := uuid.NewString()
			claim := "ag:demostore:acp:idem:update:" + id + ":" + key
			gates := stack.hold(t, "GET /api/v1/pay_way", 2)
			update := func() surfaceAnswer {
				return acpAnswer(ctx, base+"/"+id, key, otherBuyer())
			}

			first := make(chan surfaceAnswer, 1)
			go func() { first <- update() }()
			await(t, gates[0], "the first update")
			stack.lapse(t, lockRedisKey(id))
			stack.lapse(t, claim)
			holder := make(chan surfaceAnswer, 1)
			go func() { holder <- update() }()
			await(t, gates[1], "the update that holds the key")
			held, err := stack.rdb.Get(ctx, claim).Result()
			require.NoError(t, err)

			// The first update's lookup fails: it frees "its" key.
			stack.faults.payWayLookup.Store(true)
			defer stack.faults.payWayLookup.Store(false)
			gates[0].open()
			assert.Equal(t, "503 upstream_unavailable", (<-first).refusal)
			now, err := stack.rdb.Get(ctx, claim).Result()
			require.NoError(t, err, "the holder's claim was released")
			assert.Equal(t, held, now)

			stack.faults.payWayLookup.Store(false)
			gates[1].open()
			assert.NotEmpty(t, (<-holder).status, "the holder's update")
			record, err := stack.rdb.Get(ctx, claim).Result()
			require.NoError(t, err)
			assert.Contains(t, record, "bodyHash",
				"the holder recorded its key")
		})

	t.Run("succeeding, it does not record over the holder's response",
		func(t *testing.T) {
			key := uuid.NewString()
			claim := "ag:demostore:acp:idem:create.v2:" + key
			body := map[string]any{
				"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
				"currency":     "EUR",
				"capabilities": map[string]any{},
			}
			sessions := stack.sessionCount(t)
			// A create's first call to the store: the cart it mints.
			held := stack.hold(t, "GET /api/v1/cart", 1)[0]
			first := make(chan rawAnswer, 1)
			req := acpRequest(t, http.MethodPost, base, body, key)
			go func() { first <- acpSend(req) }()
			await(t, held, "the first create")
			stack.lapse(t, claim)

			resp, original := acpRaw(t,
				acpRequest(t, http.MethodPost, base, body, key))
			require.Equal(t, http.StatusCreated, resp.StatusCode, "%s",
				original)
			record, err := stack.rdb.Get(ctx, claim).Result()
			require.NoError(t, err)

			held.open()
			late := <-first
			require.NoError(t, late.err)
			// One key, one session: the late create must not hand out a
			// second one, nor leave it behind.
			assert.Equal(t, http.StatusConflict, late.status, "%s", late.body)
			var refusal map[string]any
			require.NoError(t, json.Unmarshal(late.body, &refusal))
			assert.Equal(t, "idempotency_in_flight", refusal["code"])
			now, err := stack.rdb.Get(ctx, claim).Result()
			require.NoError(t, err)
			assert.Equal(t, record, now, "the holder's record was replaced")
			assert.Equal(t, sessions+1, stack.sessionCount(t))

			resp, replayed := acpRaw(t,
				acpRequest(t, http.MethodPost, base, body, key))
			assert.Equal(t, http.StatusCreated, resp.StatusCode)
			assert.Equal(t, "true", resp.Header.Get("Idempotent-Replayed"))
			assert.Equal(t, string(original), string(replayed))
		})
}

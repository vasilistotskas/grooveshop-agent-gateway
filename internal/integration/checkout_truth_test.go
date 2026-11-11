//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
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

// The reads a call makes of its session, counted from the call's start:
// its load, then the read at its exit when its answer is made from the
// snapshot. An update that writes the cart reads once more in between,
// ahead of its first cart write.
const (
	exitRead         = 2
	beforeCartWrites = 2
	exitReadOfLines  = 3
)

// onSessionRead runs act ahead of the nth read of session id from now
// on. An error act returns is the read's result; after a nil the read
// goes on.
func (stack ucpStack) onSessionRead(
	t *testing.T, id string, nth int32, act func() error,
) {
	t.Helper()
	var reads atomic.Int32
	stack.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
		args := cmd.Args()
		if cmd.Name() != "get" || len(args) < 2 ||
			args[1] != sessionRedisKey(id) || reads.Add(1) != nth {
			return nil
		}
		return act()
	}))
	t.Cleanup(func() { reads.Store(nth) })
}

// evicted removes a session the way Redis evicts a key.
func (stack ucpStack) evicted(id string) func() error {
	return func() error {
		return stack.rdb.Del(context.Background(), sessionRedisKey(id)).Err()
	}
}

var errRedisDown = errors.New("redis is down")

// What each surface answers for a session that is no longer stored, and
// for one that cannot be read.
var (
	sessionGone = map[string]string{
		"acp": "404 not_found", "ucp": "an order may exist",
	}
	sessionUnreadable = map[string]string{
		"acp": "503 unavailable", "ucp": `"code":"unavailable"`,
	}
)

// A complete asks Django about the cart before it marks the session in
// progress, and may have outlived its lock by then. If another complete
// placed the order meanwhile, Django's answer is that order's doing: the
// cart is empty (it went with a cash-on-delivery order), or the stock is
// short (the order took the last units). Told that, the caller would
// "correct" a checkout that has an order. It is told the session
// changed, and reads it again.
func TestACompleteRefusedForAnotherCompletesOrderReadsAgain(t *testing.T) {
	for _, name := range []string{"acp", "ucp"} {
		t.Run("stock the winner's order took, over "+name, func(t *testing.T) {
			ctx := context.Background()
			stack := startUCPGateway(t)
			surfaces := surfacesByName(t, stack)
			surface := surfaces[name]
			id := surface.ready(t)
			held := stack.hold(t, "POST /api/v1/cart/reserve-stock", 1)[0]
			first := make(chan surfaceAnswer, 1)
			go func() {
				first <- surface.answer(ctx, id, uuid.NewString(), nil)
			}()
			await(t, held, "the first complete")
			stack.lapse(t, lockRedisKey(id))
			// The winner pays on the store's hosted page: its order
			// keeps the cart and awaits payment.
			require.Equal(t, "requires_escalation", surfaces["ucp"].complete(
				ctx, id, uuid.NewString(), changeBy("ucp")))
			placed := stack.storedBytes(t, id)
			stack.faults.stockShort.Store(true)

			held.open()
			assertStored(t, <-first, "requires_escalation")
			assert.Equal(t, placed, stack.storedBytes(t, id))
			assert.EqualValues(t, 1, stack.faults.orderCreates.Load())
		})
	}

	// The session is read again to tell. One that is gone by then, or
	// cannot be read, is answered as that, never with Django's refusal.
	stack := startUCPGateway(t)
	ctx := context.Background()
	surfaces := surfacesByName(t, stack)
	stack.faults.stockShort.Store(true)
	t.Cleanup(func() { stack.faults.stockShort.Store(false) })
	for name, surface := range surfaces {
		t.Run("a session evicted meanwhile, over "+name, func(t *testing.T) {
			id := surface.ready(t)
			stack.onSessionRead(t, id, exitRead, stack.evicted(id))
			answer := surface.answer(ctx, id, uuid.NewString(), nil)
			assert.Empty(t, answer.status)
			assert.Contains(t, answer.refusal, sessionGone[name])
			assert.NotContains(t, answer.refusal, "stock")
		})
		t.Run("a session that cannot be read, over "+name, func(t *testing.T) {
			id := surface.ready(t)
			stack.onSessionRead(t, id, exitRead,
				func() error { return errRedisDown })
			answer := surface.answer(ctx, id, uuid.NewString(), nil)
			assert.Empty(t, answer.status)
			assert.Contains(t, answer.refusal, "unavailable")
			assert.NotContains(t, answer.refusal, "stock")
		})
		// Unchanged, the refusal is the session's own: the stock is
		// short, and the caller is told so.
		t.Run("a session nobody touched, over "+name, func(t *testing.T) {
			id := surface.ready(t)
			answer := surface.answer(ctx, id, uuid.NewString(), nil)
			assert.Contains(t, answer.refusal, "stock")
		})
	}
}

// An update whose cart cannot be reached reads the session again, and
// answers "the cart is gone" only for a session it read and found open.
// Redis evicts session keys, so "not stored any more" is an ordinary
// answer to that read: the order may exist, and the caller is not told
// to start a new checkout.
func TestAnUpdateThatCannotReadItsSessionAgainDoesNotCallTheCartGone(
	t *testing.T,
) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surfaces := surfacesByName(t, stack)
	type scene struct {
		name, at string
		read     int32
		id       string
	}
	var scenes []scene
	for _, name := range []string{"acp", "ucp"} {
		for at, read := range map[string]int32{
			"before its cart writes": beforeCartWrites,
			"at its exit":            exitReadOfLines,
		} {
			for range 2 {
				scenes = append(scenes, scene{
					name: name, at: at, read: read,
					id: surfaces["ucp"].ready(t),
				})
			}
		}
	}
	// Another checkout's order takes the cart they all share.
	other := surfaces["ucp"].ready(t)
	stack.faults.cartDeletedOnOrder.Store(true)
	require.Equal(t, "completed", surfaces["ucp"].complete(
		ctx, other, uuid.NewString(), nil))

	for i := 0; i < len(scenes); i += 2 {
		gone, unreadable := scenes[i], scenes[i+1]
		over := ", " + gone.at + ", over " + gone.name
		t.Run("evicted"+over, func(t *testing.T) {
			stack.onSessionRead(t, gone.id, gone.read,
				stack.evicted(gone.id))
			answer := surfaces[gone.name].updateLines(ctx, gone.id)
			assert.Contains(t, answer.refusal, sessionGone[gone.name])
			assert.NotContains(t, answer.refusal, "cart no longer exists")
		})
		t.Run("unreadable"+over, func(t *testing.T) {
			stack.onSessionRead(t, unreadable.id, unreadable.read,
				func() error { return errRedisDown })
			answer := surfaces[unreadable.name].updateLines(
				ctx, unreadable.id)
			assert.Contains(t, answer.refusal,
				sessionUnreadable[unreadable.name])
			assert.NotContains(t, answer.refusal, "create_checkout")
		})
	}
}

// A session whose order is being placed, or was never confirmed, may
// have an order it does not name. Every operation answers that, on both
// surfaces: "in flight" while the completion can still be running, and
// once it cannot, that the outcome was never recorded, with what a
// person can do about it. No answer says to read again until it
// renders, and none offers a link that starts the purchase again.
func TestAnUnconfirmedOrderIsAnsweredAsOne(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surfaces := surfacesByName(t, stack)
	id := surfaces["ucp"].ready(t)
	stack.faults.orderCreate.Store(true)
	require.Empty(t, surfaces["ucp"].complete(ctx, id, uuid.NewString(), nil))
	stack.faults.orderCreate.Store(false)
	require.Equal(t, checkout.StatusCompleteInProgress,
		stack.storedSession(t, id).Status)

	t.Run("acp offers no link back to the cart", func(t *testing.T) {
		resp, body := acpCall(t, http.MethodGet,
			stack.gw.URL+"/acp/checkout_sessions/"+id, nil, "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotContains(t, body, "continue_url")
	})

	// Its cart cannot be read: it may have gone with the order.
	stack.faults.namedCartReadsFailFrom.Store(
		stack.faults.namedCartReads.Load() + 1)
	t.Cleanup(func() { stack.faults.namedCartReadsFailFrom.Store(0) })
	calls := func(surface completionSurface) map[string]surfaceAnswer {
		return map[string]surfaceAnswer{
			"read":     surface.read(ctx, id),
			"update":   surface.update(ctx, id),
			"complete": surface.answer(ctx, id, uuid.NewString(), nil),
			"cancel":   surface.abandon(ctx, id),
		}
	}
	t.Run("while its completion may still run", func(t *testing.T) {
		for what, answer := range calls(surfaces["acp"]) {
			assert.Equal(t, "409 idempotency_in_flight", answer.refusal, what)
		}
		for what, answer := range calls(surfaces["ucp"]) {
			assert.Contains(t, answer.refusal,
				"in progress or unconfirmed", what)
		}
	})

	// Past its budget the completion cannot be running any more.
	stored := stack.storedSession(t, id)
	stored.UpdatedAt = time.Now().Add(-time.Minute)
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, stack.rdb.Set(ctx, sessionRedisKey(id), raw,
		redis.KeepTTL).Err())
	t.Run("once it cannot be running", func(t *testing.T) {
		for what, answer := range calls(surfaces["acp"]) {
			assert.Equal(t, "502 order_outcome_unknown", answer.refusal, what)
			assert.Empty(t, answer.retryAfter, what)
		}
		for what, answer := range calls(surfaces["ucp"]) {
			assert.Contains(t, answer.refusal, "never confirmed", what)
			assert.Contains(t, answer.refusal,
				"order confirmation email", what)
			assert.Contains(t, answer.refusal,
				"do not start another checkout", what)
			assert.NotContains(t, answer.refusal, "check its status", what)
		}
	})

	// A read that renders carries the same guidance beside the status.
	stack.faults.namedCartReadsFailFrom.Store(0)
	read := surfaces["ucp"].read(ctx, id)
	assert.Equal(t, "complete_in_progress", read.status, read.refusal)
	assert.Contains(t, read.text, "never confirmed")
	assert.Contains(t, read.text, "order confirmation email")
	assert.EqualValues(t, 1, stack.faults.orderCreates.Load())
}

// A canceled checkout answers what it is to a complete, on UCP as on
// ACP: not that it is "not ready" and missing nothing.
func TestCompleteOfACanceledCheckoutSaysItIsCanceled(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surfaces := surfacesByName(t, stack)
	id := surfaces["ucp"].ready(t)
	surfaces["ucp"].cancel(t, id)

	answer := surfaces["ucp"].answer(ctx, id, uuid.NewString(), nil)
	assert.Contains(t, answer.refusal, "is canceled and cannot be completed")
	assert.Equal(t, "405 conflict",
		surfaces["acp"].answer(ctx, id, uuid.NewString(), nil).refusal)
}

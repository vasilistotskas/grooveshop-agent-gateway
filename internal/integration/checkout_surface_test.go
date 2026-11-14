//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// keysWritten records, while on, every command but a read that names a
// key containing one of the watched session ids.
type keysWritten struct {
	on    atomic.Bool
	mu    sync.Mutex
	ids   []string
	names []string
}

func (k *keysWritten) step(cmd redis.Cmder) error {
	if !k.on.Load() || cmd.Name() == "get" {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, arg := range cmd.Args() {
		s, ok := arg.(string)
		if !ok {
			continue
		}
		for _, id := range k.ids {
			if strings.Contains(s, id) {
				k.names = append(k.names, cmd.Name()+" "+s)
			}
		}
	}
	return nil
}

func (k *keysWritten) seen() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.names...)
}

// A checkout session answers only on the surface that created it. A UCP
// tool given an ACP session's id, and an ACP route given a UCP session's,
// answer as for an id no session is stored for. The session is neither
// written nor locked, so its own surface is not kept waiting, and it
// goes on as before there.
func TestASessionAnswersOnlyOnItsOwnSurface(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surfaces := surfacesByName(t, stack)
	ids := map[string]string{
		"acp": surfaces["acp"].ready(t),
		"ucp": surfaces["ucp"].ready(t),
	}
	other := map[string]string{"acp": "ucp", "ucp": "acp"}
	written := &keysWritten{ids: []string{ids["acp"], ids["ucp"]}}
	stack.rdb.AddHook(redisStep(written.step))

	for name, surface := range surfaces {
		id := ids[other[name]]
		before := stack.storedBytes(t, id)
		written.on.Store(true)
		answers := map[string]surfaceAnswer{
			"read":     surface.read(ctx, id),
			"update":   surface.update(ctx, id),
			"lines":    surface.updateLines(ctx, id),
			"complete": surface.answer(ctx, id, uuid.NewString(), nil),
			"cancel":   surface.abandon(ctx, id),
		}
		written.on.Store(false)
		for what, answer := range answers {
			assert.Empty(t, answer.status, "%s on %s", what, name)
			assert.Contains(t, answer.refusal, sessionGone[name],
				"%s on %s", what, name)
		}
		assert.Equal(t, before, stack.storedBytes(t, id), name)
		n, err := stack.rdb.Exists(ctx, lockRedisKey(id)).Result()
		require.NoError(t, err)
		assert.Zero(t, n, "a lock is left on %s's session", other[name])
	}
	// Not one key of either session was written, the lock included. ACP
	// claims the Idempotency-Key of an update or a cancel before it
	// loads the session, in its own family, and frees it again.
	for _, name := range written.seen() {
		assert.Contains(t, name, ":acp:idem:", "written: %s", name)
	}
	keys, err := stack.rdb.Keys(ctx, "ag:demostore:acp:idem:*").Result()
	require.NoError(t, err)
	for _, key := range keys {
		assert.NotContains(t, key, ids["ucp"], "a key left: %s", key)
	}

	ready := map[string]string{
		"acp": "ready_for_payment", "ucp": "ready_for_complete",
	}
	for name, surface := range surfaces {
		assert.Equal(t, ready[name], surface.read(ctx, ids[name]).status,
			name)
		assert.Equal(t, "completed", surface.complete(ctx, ids[name],
			uuid.NewString(), nil), name)
	}
}

// An order event reaches its checkout through the order's link, which
// names a checkout of either protocol.
func TestAnOrderEventFindsACheckoutOfEitherProtocol(t *testing.T) {
	for _, name := range []string{"acp", "ucp"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			stack := startUCPGateway(t)
			surface := surfacesByName(t, stack)[name]
			id := surface.ready(t)
			require.Equal(t, "completed",
				surface.complete(ctx, id, uuid.NewString(), nil))
			// As after a completion whose last save was lost: in progress,
			// naming no order. The order's link names the checkout.
			stack.become(t, id, func(s *checkout.Session) {
				s.Status = checkout.StatusCompleteInProgress
				s.OrderID, s.OrderUUID = 0, ""
				s.PlacedAt = time.Time{}
			})
			// The session's day counts from the order's creation.
			placed := time.Now()
			stack.faults.orderCreatedAt.Store(&placed)

			require.Equal(t, http.StatusNoContent,
				stack.pushOrderEvent(t, "COMPLETED"))

			s := stack.storedSession(t, id)
			assert.Equal(t, name, s.Protocol)
			assert.Equal(t, checkout.StatusCompleted, s.Status)
			assert.Equal(t, fixtureOrderUUID, s.OrderUUID)
		})
	}
}

// A cart's holder is read whatever surface created it: the cart rule is
// the same on both.
func TestACartsHolderIsReadOnEitherProtocol(t *testing.T) {
	ctx := context.Background()
	store, _ := startStore(t)
	for _, protocol := range []string{
		checkout.ProtocolACP, checkout.ProtocolUCP,
	} {
		holder := checkout.NewSession("demostore", "shop.test", protocol,
			uuid.NewString())
		require.NoError(t, store.Create(ctx, holder, checkout.CartHolder{}))

		seen, err := store.HolderOf(ctx, holder.Schema, holder.CartID)
		require.NoError(t, err)
		require.NotNil(t, seen.Session, protocol)
		assert.Equal(t, holder.ID, seen.Session.ID, protocol)
		assert.Equal(t, protocol, seen.Session.Protocol)
		assert.False(t, seen.Free(), protocol)
	}
}

// ACP completes only with a collect-later pay way, checked again on the
// pay ways the store answers when the order is placed. A pay way that
// reads as settled online by then places no order: the complete is
// refused, the session stays ready, and its key is free for the same
// call.
func TestACPCompletesOnlyWithACollectLaterPayWay(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surface := surfacesByName(t, stack)["acp"]
	id := surface.ready(t)
	creates := stack.faults.orderCreates.Load()
	key := uuid.NewString()
	// The complete reads the pay ways twice: to choose one, then when
	// the order is about to be placed. Only the second reads online.
	stack.faults.codOnlineFrom.Store(stack.faults.payWayReads.Load() + 2)

	answer := surface.answer(ctx, id, key, nil)

	assert.Equal(t, "400 unsupported", answer.refusal)
	assert.Equal(t, creates, stack.faults.orderCreates.Load())
	assert.Equal(t, checkout.StatusReadyForComplete,
		stack.storedSession(t, id).Status)
	n, err := stack.rdb.Exists(ctx, surface.keyName(id, key)).Result()
	require.NoError(t, err)
	assert.Zero(t, n, "the key is still held")

	stack.faults.codOnlineFrom.Store(0)
	assert.Equal(t, "completed", surface.complete(ctx, id, key, nil))
	assert.Equal(t, creates+1, stack.faults.orderCreates.Load())
}

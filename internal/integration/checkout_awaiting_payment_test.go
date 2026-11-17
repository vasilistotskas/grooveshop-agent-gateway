//go:build integration

package integration

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// A checkout awaiting its payment takes what it is from its order: it
// expires when the store will have decided the order, and is canceled
// with it or completed with its payment, by a read or by an event.
func TestACheckoutAwaitingItsPaymentFollowsItsOrder(t *testing.T) {
	stack := startUCPGateway(t)
	ctx := t.Context()
	ucpSurface := surfacesByName(t, stack)["ucp"]
	event := func(t *testing.T, orderStatus, paymentStatus string) {
		t.Helper()
		status, err := stack.orderStatusEvent(orderStatus, paymentStatus)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, status)
	}

	t.Run("it expires when the store will have decided its order",
		func(t *testing.T) {
			id := awaitingPayment(t, ucpSurface)
			stored := stack.storedSession(t, id)

			// The fake decides an online order 50 minutes after a read.
			assert.WithinDuration(t, time.Now().Add(50*time.Minute),
				stored.ExpiresAt, time.Minute)
			ttl, err := stack.rdb.PTTL(ctx, sessionRedisKey(id)).Result()
			require.NoError(t, err)
			assert.GreaterOrEqual(t, ttl, time.Until(stored.ExpiresAt),
				"readable until its expiry")
		})

	// A read publishes only what it stored: an expiry it could not
	// store is not given, or a later read could give an earlier one.
	t.Run("an expiry that cannot be stored is not published",
		func(t *testing.T) {
			id := awaitingPayment(t, ucpSurface)
			before := stack.storedSession(t, id).ExpiresAt
			// The fake's decision moves with the clock, by the second.
			time.Sleep(1100 * time.Millisecond)
			var down atomic.Bool
			down.Store(true)
			stack.rdb.AddHook(redisStep(func(cmd redis.Cmder) error {
				if down.Load() && savesKey(cmd, sessionRedisKey(id)) {
					return errors.New("redis is down")
				}
				return nil
			}))
			t.Cleanup(func() { down.Store(false) })

			a := ucpSurface.read(ctx, id)

			assert.Empty(t, a.status)
			assert.Contains(t, a.refusal, "order_placed")
			assert.True(t, stack.storedSession(t, id).ExpiresAt.Equal(before))
		})

	t.Run("a read finds its order canceled", func(t *testing.T) {
		id := awaitingPayment(t, ucpSurface)
		stack.faults.orderCanceled.Store(true)
		t.Cleanup(func() { stack.faults.orderCanceled.Store(false) })

		assert.Equal(t, "canceled", ucpSurface.read(ctx, id).status)
		assert.Equal(t, checkout.StatusCanceled,
			stack.storedSession(t, id).Status, "stored as it read")
	})

	t.Run("an event cancels it with its order", func(t *testing.T) {
		id := awaitingPayment(t, ucpSurface)

		event(t, django.OrderStatusCanceled, "CANCELED")

		assert.Equal(t, checkout.StatusCanceled,
			stack.storedSession(t, id).Status)
	})

	t.Run("a payment after the cancel leaves it canceled",
		func(t *testing.T) {
			id := awaitingPayment(t, ucpSurface)

			event(t, django.OrderStatusCanceled,
				django.PaymentStatusCompleted)

			assert.Equal(t, checkout.StatusCanceled,
				stack.storedSession(t, id).Status)
		})

	t.Run("a payment completes it however late", func(t *testing.T) {
		id := awaitingPayment(t, ucpSurface)
		// It is kept past an open checkout's half hour, until its order
		// is decided, so a payment that lands late still finds it.
		s := stack.storedSession(t, id)
		require.Greater(t, s.ExpiresAt.Sub(s.UpdatedAt), 30*time.Minute)

		event(t, "PROCESSING", django.PaymentStatusCompleted)

		assert.Equal(t, checkout.StatusCompleted,
			stack.storedSession(t, id).Status)
	})
}

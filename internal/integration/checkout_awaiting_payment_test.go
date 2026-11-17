//go:build integration

package integration

import (
	"net/http"
	"testing"
	"time"

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

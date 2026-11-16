//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

// The UCP idempotency key is 1 to 255 visible ASCII characters: a UUID,
// or any other key of that form, places one order and replays it. A key
// outside that form is refused before anything is written, and the
// checkout is left as it was.
func TestTheUCPIdempotencyKeyIsBounded(t *testing.T) {
	ctx := t.Context()
	stack := startUCPGateway(t)
	surface := surfacesByName(t, stack)["ucp"]

	p := newPlatform(t, stack)
	cancel := func(id, key string) surfaceAnswer {
		return mcpAnswer(ctx, p.session, "cancel_checkout",
			map[string]any{"meta": p.meta(key), "id": id})
	}

	t.Run("a key outside the bound", func(t *testing.T) {
		id := surface.ready(t)
		before := stack.storedBytes(t, id)
		for _, key := range []string{
			strings.Repeat("k", checkout.MaxIdempotencyKeyLen+1),
			"a key with spaces",
			"κλειδί",
		} {
			for what, answer := range map[string]surfaceAnswer{
				"complete": surface.answer(ctx, id, key, nil),
				"cancel":   cancel(id, key),
			} {
				assert.Empty(t, answer.status, what)
				assert.Contains(t, answer.refusal,
					"1 to 255 visible ASCII characters", what)
			}
		}
		assert.Equal(t, before, stack.storedBytes(t, id))
		keys, err := stack.rdb.Keys(ctx, "ag:demostore:idem:*").Result()
		require.NoError(t, err)
		assert.Empty(t, keys)
		n, err := stack.rdb.Exists(ctx, lockRedisKey(id)).Result()
		require.NoError(t, err)
		assert.Zero(t, n)

		assert.Equal(t, "canceled",
			cancel(id, strings.Repeat("k", 255)).status)
	})

	for name, key := range map[string]string{
		"a UUID":                       uuid.NewString(),
		"22 alphanumeric characters":   "Zk3pQ9wL2mX7vB4nR8tY1c",
		"255 visible ASCII characters": strings.Repeat("~", 255),
		"other visible ASCII":          "order-1:retry_2/a.b+c=d",
	} {
		t.Run(name, func(t *testing.T) {
			id := surface.ready(t)
			creates := stack.faults.orderCreates.Load()
			require.Equal(t, "completed", surface.complete(ctx, id, key, nil))
			require.Equal(t, "completed", surface.complete(ctx, id, key, nil))
			assert.Equal(t, creates+1, stack.faults.orderCreates.Load())
		})
	}
}

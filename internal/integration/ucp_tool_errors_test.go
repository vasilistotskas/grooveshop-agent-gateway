//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
)

// toolError decodes a UCP tool's refusal as the gateway's ToolError, the
// shape every failure an agent acts on by its kind takes.
func toolError(t *testing.T, refusal string) mcptool.ToolError {
	t.Helper()
	var te mcptool.ToolError
	require.NoError(t, json.Unmarshal([]byte(refusal), &te),
		"not a ToolError: %s", refusal)
	return te
}

// A checkout id no session is stored for, and a checkout another request
// holds, are answered with a code an agent can branch on: not_found and
// conflict. The missing one still says an order may exist; the busy one
// that this call did nothing.
func TestUCPCheckoutAnswersCarryACode(t *testing.T) {
	ctx := context.Background()
	stack := startUCPGateway(t)
	surface := surfacesByName(t, stack)["ucp"]

	t.Run("a checkout no session is stored for", func(t *testing.T) {
		id := uuid.NewString()
		for what, answer := range map[string]surfaceAnswer{
			"read":     surface.read(ctx, id),
			"update":   surface.update(ctx, id),
			"complete": surface.answer(ctx, id, uuid.NewString(), nil),
			"cancel":   surface.abandon(ctx, id),
		} {
			te := toolError(t, answer.refusal)
			assert.Equal(t, mcptool.CodeNotFound, te.Code, what)
			assert.Contains(t, te.Hint, "an order may exist", what)
			assert.Contains(t, te.Hint,
				"do not start another checkout before checking", what)
		}
	})

	t.Run("a checkout another request holds", func(t *testing.T) {
		id := surface.ready(t)
		before := stack.storedBytes(t, id)
		require.NoError(t, stack.rdb.Set(ctx, lockRedisKey(id),
			"another request", 10*time.Second).Err())
		t.Cleanup(func() {
			_ = stack.rdb.Del(ctx, lockRedisKey(id)).Err()
		})
		for what, answer := range map[string]surfaceAnswer{
			"update":   surface.update(ctx, id),
			"complete": surface.answer(ctx, id, uuid.NewString(), nil),
			"cancel":   surface.abandon(ctx, id),
		} {
			te := toolError(t, answer.refusal)
			assert.Equal(t, mcptool.CodeConflict, te.Code, what)
			assert.Contains(t, te.Hint, "this call did nothing", what)
		}
		assert.Equal(t, before, stack.storedBytes(t, id))
		// A read takes no lock.
		assert.Equal(t, "ready_for_complete", surface.read(ctx, id).status)
	})
}

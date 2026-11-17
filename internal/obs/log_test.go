package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for raw := range strings.Lines(buf.String()) {
		var line map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &line))
		out = append(out, line)
	}
	return out
}

func TestContextHandlerAddsRequestIDAndTenant(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))

	ctx := WithTenantSlot(WithRequestID(t.Context(), "req-1"))
	log.InfoContext(ctx, "before the tenant is known")
	SetTenant(ctx, "demostore")
	log.InfoContext(ctx, "plain")
	log.With(slog.String("tool", "search")).InfoContext(ctx, "with")
	log.WithGroup("call").InfoContext(ctx, "group", slog.Int("n", 1))
	log.InfoContext(t.Context(), "no request")
	log.InfoContext(ctx, "names its own",
		slog.String(KeyTenant, "acme"))

	got := lines(t, &buf)
	require.Len(t, got, 6)

	assert.Equal(t, "req-1", got[0][KeyRequestID])
	assert.NotContains(t, got[0], KeyTenant)

	for _, line := range got[1:3] {
		assert.Equal(t, "req-1", line[KeyRequestID], line["msg"])
		assert.Equal(t, "demostore", line[KeyTenant], line["msg"])
	}
	assert.Equal(t, "search", got[2]["tool"])

	// A record's attributes belong to the open group, so these do too.
	group, ok := got[3]["call"].(map[string]any)
	require.True(t, ok, got[3])
	assert.Equal(t, "req-1", group[KeyRequestID])
	assert.Equal(t, "demostore", group[KeyTenant])

	assert.NotContains(t, got[4], KeyRequestID)
	assert.NotContains(t, got[4], KeyTenant)

	assert.Equal(t, "acme", got[5][KeyTenant])
	assert.Equal(t, 1, strings.Count(
		strings.Split(buf.String(), "\n")[5], `"tenant"`))
}

// Work a request detaches from its cancellation (a completion that
// outlives its caller, a singleflight fetch) still logs under the
// request's id and tenant, a recovered panic included.
func TestContextHandlerFollowsADetachedContext(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))
	reqCtx, cancel := context.WithCancel(
		WithTenantSlot(WithRequestID(t.Context(), "req-1")))
	detached := context.WithoutCancel(reqCtx)
	cancel()
	SetTenant(reqCtx, "demostore")

	err := func() (err error) {
		defer Recover(detached, log, "op", &err)
		panic("boom")
	}()

	require.Error(t, err)
	got := lines(t, &buf)
	require.Len(t, got, 1)
	assert.Equal(t, "panic recovered", got[0]["msg"])
	assert.Equal(t, "req-1", got[0][KeyRequestID])
	assert.Equal(t, "demostore", got[0][KeyTenant])
}

func TestContextHandlerKeepsTheLevel(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewContextHandler(slog.NewJSONHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelWarn})))
	log.InfoContext(WithRequestID(t.Context(), "req-1"), "dropped")
	assert.Empty(t, buf.String())
}

// go-redis's lines are the gateway's JSON at warn, and one go-redis logs
// with a request's context names the request and store.
func TestRedisLoggerWritesTheGatewaysLines(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := WithTenantSlot(WithRequestID(t.Context(), "req-1"))
	SetTenant(ctx, "demostore")

	RedisLogger{Log: log}.Printf(ctx, "redis: %s after %d attempts",
		"failed to dial", 3)

	got := lines(t, &buf)
	require.Len(t, got, 1)
	assert.Equal(t, "WARN", got[0]["level"])
	assert.Equal(t, "redis client", got[0]["msg"])
	assert.Equal(t, "redis: failed to dial after 3 attempts",
		got[0]["detail"])
	assert.Equal(t, "req-1", got[0][KeyRequestID])
	assert.Equal(t, "demostore", got[0][KeyTenant])
}

// The logger is JSON from its first line, at the level a LevelVar names
// once it is set.
func TestNewLoggerFollowsItsLevel(t *testing.T) {
	var buf bytes.Buffer
	level := new(slog.LevelVar)
	log := NewLogger(&buf, level, "v1")

	log.DebugContext(t.Context(), "dropped at info")
	level.Set(slog.LevelDebug)
	log.DebugContext(t.Context(), "kept at debug")

	got := lines(t, &buf)
	require.Len(t, got, 1)
	assert.Equal(t, "kept at debug", got[0]["msg"])
	assert.Equal(t, "agent-gateway", got[0]["service"])
	assert.Equal(t, "v1", got[0]["version"])
}

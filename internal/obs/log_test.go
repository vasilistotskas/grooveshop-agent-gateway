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

	ctx := WithTenantSlot(WithRequestID(context.Background(), "req-1"))
	log.InfoContext(ctx, "before the tenant is known")
	SetTenant(ctx, "demostore")
	log.InfoContext(ctx, "plain")
	log.With(slog.String("tool", "search")).InfoContext(ctx, "with")
	log.WithGroup("call").InfoContext(ctx, "group", slog.Int("n", 1))
	log.InfoContext(context.Background(), "no request")
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

func TestContextHandlerKeepsTheLevel(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewContextHandler(slog.NewJSONHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelWarn})))
	log.InfoContext(WithRequestID(context.Background(), "req-1"), "dropped")
	assert.Empty(t, buf.String())
}

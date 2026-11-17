package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// jsonLines decodes every line of out, failing on one that is not JSON.
func jsonLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for raw := range strings.Lines(out) {
		var line map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &line), raw)
		lines = append(lines, line)
	}
	return lines
}

// A configuration the gateway refuses ends the process with exit code 1
// and one JSON line, through the handler every other line takes.
func TestABootFailureIsOneJSONLine(t *testing.T) {
	t.Setenv("LOG_LEVEL", "loud")
	var out bytes.Buffer

	code := start(&out, nil)

	assert.Equal(t, 1, code)
	lines := jsonLines(t, out.String())
	require.Len(t, lines, 1, out.String())
	assert.Equal(t, "ERROR", lines[0]["level"])
	assert.Equal(t, "fatal", lines[0]["msg"])
	assert.Equal(t, "agent-gateway", lines[0]["service"])
	assert.Equal(t, version, lines[0]["version"])
	assert.Contains(t, lines[0]["error"], "LOG_LEVEL")
}

// go-redis's own lines go through the gateway's logger as JSON, at warn,
// instead of plain text on stderr. A dial's runs on a context of
// go-redis's own, so it names no request.
func TestRedisClientLinesAreJSON(t *testing.T) {
	// A port nothing listens on: every dial is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	var out lockedBuffer
	log := slog.New(obs.NewContextHandler(slog.NewJSONHandler(&out, nil)))
	rdb, err := newRedis("redis://"+addr, log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rdb.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.Error(t, rdb.Ping(ctx).Err())

	var redisLines []map[string]any
	for _, line := range jsonLines(t, out.String()) {
		if line["msg"] == "redis client" {
			redisLines = append(redisLines, line)
		}
	}
	require.NotEmpty(t, redisLines, out.String())
	line := redisLines[0]
	assert.Equal(t, "WARN", line["level"])
	assert.Contains(t, line["detail"], "failed to dial")
}

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	redislog "github.com/redis/go-redis/v9/logging"
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

// A failure after the configuration loaded is logged with the
// environment it names. A REDIS_URL that does not parse is that failure,
// and its line never carries the URL's password.
func TestAFailureAfterTheConfigurationNamesItsEnvironment(t *testing.T) {
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	for name, value := range map[string]string{
		"ENV":                        "test",
		"DJANGO_BASE_URL":            "http://backend.test/api/v1",
		"DJANGO_PUBLIC_HOST":         "api.example.test",
		"REDIS_URL":                  "redis://:s3cr%zzet@redis.test:6379/0",
		"INTERNAL_EVENTS_SECRET":     "internal-secret",
		"ASSETS_HOST":                "assets.test",
		"STAFF_CONNECT_HOST":         "connect.test",
		"STOREFRONT_DEFAULT_LOCALE":  "el",
		"SIGNING_KEY_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(kek),
	} {
		t.Setenv(name, value)
	}
	var out bytes.Buffer

	code := start(&out, nil)

	assert.Equal(t, 1, code)
	lines := jsonLines(t, out.String())
	require.Len(t, lines, 1, out.String())
	assert.Equal(t, "fatal", lines[0]["msg"])
	assert.Equal(t, "test", lines[0][obs.KeyEnv])
	assert.Contains(t, lines[0]["error"], "REDIS_URL")
	assert.NotContains(t, out.String(), "s3cr")
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
	t.Cleanup(func() {
		_ = rdb.Close()
		// go-redis's logger is the process's: give it back its default.
		redislog.Enable()
	})

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

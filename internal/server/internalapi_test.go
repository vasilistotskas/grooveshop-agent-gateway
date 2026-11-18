package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/feeds"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
)

const testSecret = "shared-internal-secret"

func quietLog() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// unreachableRedis exercises the Redis-failure path without Docker; the
// happy path (keys actually removed) is covered by the integration
// suite, which runs against a real Redis.
func unreachableRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:            "127.0.0.1:1",
		DialTimeout:     20 * time.Millisecond,
		ReadTimeout:     20 * time.Millisecond,
		WriteTimeout:    20 * time.Millisecond,
		MaxRetries:      -1,
		PoolSize:        1,
		MinIdleConns:    0,
		ConnMaxIdleTime: time.Millisecond,
	})
}

func invalidateHandler(rdb *redis.Client) http.Handler {
	svc := feeds.NewService(nil, rdb, quietLog(), obs.NewMetrics(), "", "",
		time.Hour, 24*time.Hour, "test", storefront.NewSite("el"))
	return internalFeedInvalidate(testSecret, svc, quietLog())
}

func post(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/internal/feeds/invalidate", strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The endpoint exists so a merchant's price change does not wait out
// FEED_FRESH_TTL (6h) before reaching Google, Meta and TikTok. It is
// cluster-internal, so the token check is the thing most worth pinning:
// a regression there exposes catalogue cache control to the internet.
func TestInternalFeedInvalidateAuth(t *testing.T) {
	h := invalidateHandler(unreachableRedis())

	t.Run("no token is forbidden", func(t *testing.T) {
		rec := post(t, h, "", `{"schemaName":"webside"}`)
		require.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("wrong token is forbidden", func(t *testing.T) {
		rec := post(t, h, "nope", `{"schemaName":"webside"}`)
		require.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("a token prefix is not enough", func(t *testing.T) {
		rec := post(t, h, testSecret[:5], `{"schemaName":"webside"}`)
		require.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("auth is checked before the body is read", func(t *testing.T) {
		// An unauthenticated caller must not be able to tell a malformed
		// body from a valid one.
		rec := post(t, h, "nope", `not json at all`)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestInternalFeedInvalidateValidation(t *testing.T) {
	h := invalidateHandler(unreachableRedis())

	t.Run("malformed json is a bad request", func(t *testing.T) {
		rec := post(t, h, testSecret, `{`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("missing schemaName is a bad request", func(t *testing.T) {
		// Without it there is nothing to scope the purge to, and
		// guessing would drop another tenant's feeds.
		rec := post(t, h, testSecret, `{}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "schemaName")
	})

	t.Run("empty schemaName is a bad request", func(t *testing.T) {
		rec := post(t, h, testSecret, `{"schemaName":""}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

func TestInternalFeedInvalidateRedisFailureIsRetryable(t *testing.T) {
	// 503, not 500: the caller is Django's cache-purge service, and a
	// transient Redis blip should make it retry rather than report a
	// successful purge that never happened.
	h := invalidateHandler(unreachableRedis())

	rec := post(t, h, testSecret, `{"schemaName":"webside"}`)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// An order event is Django's outbox row: without its id and time the
// webhook has no Webhook-Id and Webhook-Timestamp to carry unchanged
// across attempts, so the body is refused, which no retry fixes. The
// token is still checked first.
func TestOrderEventRequiresTheOutboxRowsIDAndTime(t *testing.T) {
	h := internalOrderEvents(testSecret, orderEventDeps{})
	send := func(token, body string) int {
		req := httptest.NewRequest(http.MethodPost,
			"/internal/events/order-status", strings.NewReader(body))
		req.Header.Set("X-Internal-Token", token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const order = `"schemaName":"demostore",` +
		`"orderUuid":"b9be45e5-6062-4976-ae7b-2c31eb2ad689"`
	const id = `"eventId":"0193a6f0-7c1e-7b3a-9c4d-5e6f7a8b9c0d"`
	const at = `"occurredAt":"2026-11-18T02:00:00.123456+00:00"`

	assert.Equal(t, http.StatusForbidden, send("nope", "{"+order+"}"))
	for name, body := range map[string]string{
		"no event id":        "{" + order + "," + at + "}",
		"an id not a UUID":   "{" + order + `,"eventId":"7",` + at + "}",
		"no event time":      "{" + order + "," + id + "}",
		"a time not RFC3339": "{" + order + "," + id + `,"occurredAt":"yesterday"}`,
		"no order":           `{"schemaName":"demostore",` + id + "," + at + "}",
		"no store":           `{"orderUuid":"b9be45e5-6062-4976-ae7b-2c31eb2ad689",` + id + "," + at + "}",
		"not JSON":           "{",
	} {
		assert.Equal(t, http.StatusBadRequest, send(testSecret, body), name)
	}
}

// Retry-After is whole seconds, rounded up, so Django never comes back
// before the wait is over.
func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	assert.Equal(t, "1", retryAfterSeconds(time.Millisecond))
	assert.Equal(t, "30", retryAfterSeconds(30*time.Second))
	assert.Equal(t, "31", retryAfterSeconds(30*time.Second+time.Nanosecond))
}

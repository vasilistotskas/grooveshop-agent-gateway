package obs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Readiness gates on Redis alone. Django's state is reported in the
// body and never flips the status: with Django down the gateway still
// answers agents with actionable errors instead of Traefik's 502.
func TestReadyzGatesOnRedisOnly(t *testing.T) {
	down := errors.New("connection refused")
	up := func(context.Context) error { return nil }
	fail := func(context.Context) error { return down }

	cases := []struct {
		name          string
		redis, django func(context.Context) error
		status        int
		body          map[string]string
	}{
		{
			name: "both up", redis: up, django: up,
			status: http.StatusOK,
			body:   map[string]string{"redis": "ok", "django": "ok"},
		},
		{
			name: "django down", redis: up, django: fail,
			status: http.StatusOK,
			body:   map[string]string{"redis": "ok", "django": down.Error()},
		},
		{
			name: "redis down", redis: fail, django: up,
			status: http.StatusServiceUnavailable,
			body:   map[string]string{"redis": down.Error(), "django": "ok"},
		},
		{
			name: "both down", redis: fail, django: fail,
			status: http.StatusServiceUnavailable,
			body: map[string]string{
				"redis": down.Error(), "django": down.Error(),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Readyz(tc.redis, tc.django).ServeHTTP(rec,
				httptest.NewRequest(http.MethodGet, "/readyz", nil))

			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, "application/json",
				rec.Header().Get("Content-Type"))
			var body map[string]string
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tc.body, body)
		})
	}
}

// Both pings run under a bound of their own, so a hung dependency
// cannot hold the kubelet's probe open.
func TestReadyzBoundsEachPing(t *testing.T) {
	var redisBound, djangoBound bool
	probe := func(seen *bool) func(context.Context) error {
		return func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			left := time.Until(deadline)
			*seen = ok && left > 0 && left <= readyzPingTimeout
			return nil
		}
	}
	rec := httptest.NewRecorder()
	Readyz(probe(&redisBound), probe(&djangoBound)).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/readyz", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, redisBound)
	assert.True(t, djangoBound)
}

func TestHealthzChecksNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	Healthz().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

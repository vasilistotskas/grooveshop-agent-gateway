package tenant

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// jsonLog collects JSON lines through the gateway's context handler.
type jsonLog struct{ capturedLog }

func (j *jsonLog) logger() *slog.Logger {
	return slog.New(obs.NewContextHandler(slog.NewJSONHandler(j, nil)))
}

func (j *jsonLog) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(j.String()))
	for sc.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		if line["msg"] == msg {
			out = append(out, line)
		}
	}
	return out
}

// Requests that share a resolve log it once, under the request that
// started it: the flight's own lines (the Django client's, the Redis
// tier's) carry the leader's id. What each request then answers is its
// own, and so is the line it logs about it: every waiter's "store
// resolution failed" carries its own id, never the leader's.
func TestASharedResolveLogsEachOutcomeUnderItsOwnRequest(t *testing.T) {
	var logs jsonLog
	log := logs.logger()
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			once.Do(func() { close(entered) })
			<-release
			// Django drops the connection: an outage.
			if conn, _, err := http.NewResponseController(w).
				Hijack(); err == nil {
				_ = conn.Close()
			}
		}))
	t.Cleanup(srv.Close)
	dj := django.New(srv.URL+"/api/v1", "api.example.test", "test-secret",
		5*time.Second, log, nil)
	r := NewResolver(dj, unreachableRedis(), time.Minute, testNegTTL,
		log, nil)
	mw := Middleware(r, log)(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {
			t.Error("no store resolves")
		}))

	serve := func(id string) <-chan int {
		done := make(chan int, 1)
		go func() {
			req := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
			req.Host = "shop.example.test"
			req = req.WithContext(obs.WithTenantSlot(
				obs.WithRequestID(t.Context(), id)))
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			done <- rec.Code
		}()
		return done
	}
	leader := serve("req-leader")
	<-entered
	follower := serve("req-follower")
	// The follower joins the leader's flight while Django holds it.
	time.Sleep(200 * time.Millisecond)
	close(release)
	assert.Equal(t, http.StatusServiceUnavailable, <-leader)
	assert.Equal(t, http.StatusServiceUnavailable, <-follower)
	require.Equal(t, int32(3), calls.Load(),
		"one resolve: one GET of three attempts")

	for _, msg := range []string{
		"tenant redis read failed", "django request failed",
	} {
		lines := logs.lines(t, msg)
		require.NotEmpty(t, lines, logs.String())
		for _, line := range lines {
			assert.Equal(t, "req-leader", line[obs.KeyRequestID], msg)
		}
	}
	var ids []any
	for _, line := range logs.lines(t, "store resolution failed") {
		ids = append(ids, line[obs.KeyRequestID])
	}
	assert.ElementsMatch(t, []any{"req-leader", "req-follower"}, ids)
}

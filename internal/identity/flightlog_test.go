package identity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// lockedJSON collects JSON lines written from several goroutines.
type lockedJSON struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedJSON) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// ids returns the request id of every line logged as msg.
func (l *lockedJSON) ids(t *testing.T, msg string) []any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []any
	sc := bufio.NewScanner(bytes.NewReader(l.buf.Bytes()))
	for sc.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
		if line["msg"] == msg {
			out = append(out, line[obs.KeyRequestID])
		}
	}
	return out
}

// Calls that share a probe log it once, under the call that started it:
// the probe's own lines (the Django client's) carry the leader's id.
// What each call then answers is its own, and so is the line it logs
// about it: every waiter's "token verification failed" carries its own
// id, never the leader's.
func TestASharedProbeLogsEachOutcomeUnderItsOwnRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs lockedJSON
		log := slog.New(obs.NewContextHandler(
			slog.NewJSONHandler(&logs, nil)))
		release := make(chan struct{})
		v := NewVerifier(func(
			ctx context.Context, _ *tenant.Tenant, _ string,
		) (*django.AgentProfile, error) {
			<-release
			log.WarnContext(ctx, "probe failed")
			return nil, django.ErrUpstreamDown
		}, log)
		h := Middleware(v, log)(http.HandlerFunc(
			func(http.ResponseWriter, *http.Request) {
				t.Error("an unverified call must not reach the handler")
			}))

		var wg sync.WaitGroup
		codes := map[string]int{}
		var mu sync.Mutex
		call := func(id string) {
			wg.Go(func() {
				ctx := tenant.NewContext(
					obs.WithRequestID(t.Context(), id), testTenant())
				req := httptest.NewRequestWithContext(
					ctx, http.MethodPost, "/mcp", nil)
				req.Header.Set("Authorization", "Bearer good")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				mu.Lock()
				codes[id] = rec.Code
				mu.Unlock()
			})
		}
		call("req-leader")
		synctest.Wait() // the leader's probe holds
		call("req-follower")
		synctest.Wait() // the follower waits on the leader's probe
		close(release)
		wg.Wait()

		assert.Equal(t, map[string]int{
			"req-leader":   http.StatusServiceUnavailable,
			"req-follower": http.StatusServiceUnavailable,
		}, codes)
		assert.Equal(t, []any{"req-leader"},
			logs.ids(t, "probe failed"), "one probe")
		assert.ElementsMatch(t, []any{"req-leader", "req-follower"},
			logs.ids(t, "token verification failed"))
	})
}

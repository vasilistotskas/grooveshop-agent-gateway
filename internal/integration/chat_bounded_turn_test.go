//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
)

// A provider that throttles the turn is asked once: the shopper learns
// at once how long to wait, and the widget's retry button does the rest.
func TestChatProviderThrottleIsAskedOnceAndSaysWhenToRetry(t *testing.T) {
	fake := &fakeChatAPI{answer: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error": {"code": 429, ` +
			`"message": "Resource exhausted.", ` +
			`"status": "RESOURCE_EXHAUSTED"}}`))
	}}
	gw := startChatGateway(t, fake)

	events := postChat(t, gw.URL, map[string]any{"message": "γεια"})

	assert.EqualValues(t, 1, fake.calls.Load(), "no hidden retries")
	failure := lastEvent(t, events, "error")
	assert.Equal(t, "rate_limited", failure["code"])
	assert.EqualValues(t, 3, failure["retryAfter"])
}

// A quota error the provider sends inside the stream is the same
// throttle as one it answers the request with.
func TestChatProviderThrottleInsideTheStreamIsRateLimited(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		`data: {"error": {"code": 429, "message": "Quota exceeded. ` +
			`Please retry in 7.5s.", "status": "RESOURCE_EXHAUSTED"}}` +
			"\n\n",
	}}
	gw := startChatGateway(t, fake)

	events := postChat(t, gw.URL, map[string]any{"message": "γεια"})

	failure := lastEvent(t, events, "error")
	assert.Equal(t, "rate_limited", failure["code"])
	assert.EqualValues(t, 8, failure["retryAfter"])
}

// A provider that stalls is cut at the turn's deadline, and the shopper
// gets the retryable failure instead of a spinner.
func TestChatStalledProviderIsCutAtTheTurnDeadline(t *testing.T) {
	release := make(chan struct{})
	fake := &fakeChatAPI{answer: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"cmpl-e2e",` +
			`"object":"chat.completion.chunk","created":1,"model":"fake",` +
			`"choices":[{"index":0,"delta":{"role":"assistant",` +
			`"content":"Ψάχνω"},"finish_reason":null}]}` + "\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}}
	logs := &lockedBuffer{}
	gw, _ := newChatGateway(t, fake, fakeDjangoMux(t),
		slog.New(slog.NewJSONHandler(logs, nil)), func(c *config.Config) {
			c.ChatTurnTimeout = 500 * time.Millisecond
		})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started := time.Now()
	events := postChatWithin(ctx, t, gw.URL, http.Header{},
		map[string]any{"message": "θήκες"})

	assert.Less(t, time.Since(started), 5*time.Second)
	failure := lastEvent(t, events, "error")
	assert.Nil(t, failure["code"], "a stall is no throttle")
	assert.Contains(t, failure["message"], "διακόπηκε")
	// The deadline is the turn's own failure, not a shutdown or a
	// shopper who left: those branches report the same event.
	assert.Contains(t, logs.String(), `"msg":"chat turn failed"`)
	assert.Contains(t, logs.String(), "context deadline exceeded")
	assert.NotContains(t, logs.String(), "chat turn cut by shutdown")
	assert.NotContains(t, logs.String(), "chat turn abandoned by client")
}

// Once the turn's deadline has passed, no further tool call of the batch
// starts: the one running finishes (a cart call is never cut midway),
// the next does not run, and the turn ends with the retryable failure.
func TestChatTurnDeadlineStopsTheToolCallsNotYetStarted(t *testing.T) {
	var batch strings.Builder
	for i, id := range []string{"call_slow", "call_next"} {
		chunk(&batch, fmt.Sprintf(`{"role":"assistant","tool_calls":[{`+
			`"index":%d,"id":%q,"type":"function","function":{"name":`+
			`"add_to_cart","arguments":"{\"productId\": 1}"}}]}`, i, id), "")
	}
	chunk(&batch, `{}`, "tool_calls")
	batch.WriteString("data: [DONE]\n\n")
	fake := &fakeChatAPI{scripts: []string{batch.String()}}

	var adds atomic.Int32
	base := fakeDjangoMux(t)
	djangoH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/cart/item" {
			// The first add outlasts the turn's deadline.
			if adds.Add(1) == 1 {
				time.Sleep(time.Second)
			}
		}
		base.ServeHTTP(w, r)
	})
	logs := &lockedBuffer{}
	gw, _ := newChatGateway(t, fake, djangoH,
		slog.New(slog.NewJSONHandler(logs, nil)), func(c *config.Config) {
			c.ChatTurnTimeout = 300 * time.Millisecond
		})

	events := postChat(t, gw.URL, map[string]any{"message": "βάλε δύο"})

	assert.EqualValues(t, 1, adds.Load(), "the queued add_to_cart ran")
	assert.Equal(t, []any{"running", "done"}, toolStatuses(events),
		"the running add finishes")
	failure := lastEvent(t, events, "error")
	assert.Nil(t, failure["code"])
	assert.Contains(t, failure["message"], "διακόπηκε")
	assert.EqualValues(t, 1, fake.calls.Load())
	assert.Contains(t, logs.String(), "chat: turn ended before tool "+
		"add_to_cart: context deadline exceeded")
}

// The last iteration must answer in prose: tool calls a provider returns
// there anyway are not run, so no cart changes behind the shopper's back.
func TestChatToolCallsOnTheLastIterationAreNotRun(t *testing.T) {
	search := toolUseTurnSSE("search_products", `{"query": "θήκη"}`)
	fake := &fakeChatAPI{scripts: []string{
		search, search, search,
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
	}}
	var adds atomic.Int32
	base := fakeDjangoMux(t)
	djangoH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/cart/item" {
			adds.Add(1)
		}
		base.ServeHTTP(w, r)
	})
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger()) // 4 iterations

	events := postChat(t, gw.URL, map[string]any{"message": "θήκες"})

	require.EqualValues(t, 4, fake.calls.Load())
	assert.Zero(t, adds.Load(), "the last iteration's add_to_cart ran")
	for _, e := range events {
		if e.name == "tool" {
			assert.Equal(t, "search_products", e.data["name"])
		}
	}
	assert.Equal(t, false, lastEvent(t, events, "done")["cartMutated"])
}

// Every turn reaches the provider through one client, so a turn reuses
// the connection the previous one opened.
func TestChatTurnsReuseTheModelConnection(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		textTurnSSE("ένα"), textTurnSSE("δύο"),
	}}
	gw := startChatGateway(t, fake)

	postChat(t, gw.URL, map[string]any{"message": "πρώτο"})
	postChat(t, gw.URL, map[string]any{"message": "δεύτερο"})

	assert.Equal(t, 1, fake.conns())
}

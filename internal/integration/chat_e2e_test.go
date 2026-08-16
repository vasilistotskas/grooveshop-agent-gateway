//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/server"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// chunk writes one OpenAI chat-completions SSE chunk (data-only frames;
// the stream terminates with data: [DONE]).
func chunk(b *strings.Builder, deltaJSON, finishReason string) {
	finish := "null"
	if finishReason != "" {
		finish = `"` + finishReason + `"`
	}
	fmt.Fprintf(b,
		`data: {"id":"cmpl-e2e","object":"chat.completion.chunk",`+
			`"created":1,"model":"fake","choices":[{"index":0,`+
			`"delta":%s,"finish_reason":%s}]}`+"\n\n",
		deltaJSON, finish)
}

// textTurnSSE is a complete streamed assistant turn of plain text.
func textTurnSSE(chunks ...string) string {
	var b strings.Builder
	chunk(&b, `{"role":"assistant","content":""}`, "")
	for _, c := range chunks {
		raw, _ := json.Marshal(c)
		chunk(&b, `{"content":`+string(raw)+`}`, "")
	}
	chunk(&b, `{}`, "stop")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// toolUseTurnSSE is a streamed assistant turn that calls one tool, with
// the arguments split across chunks the way real providers stream them.
func toolUseTurnSSE(toolName, argsJSON string) string {
	var b strings.Builder
	chunk(&b, `{"role":"assistant","tool_calls":[{"index":0,`+
		`"id":"call_e2e_1","type":"function","function":{"name":"`+
		toolName+`","arguments":""}}]}`, "")
	half := len(argsJSON) / 2
	for _, part := range []string{argsJSON[:half], argsJSON[half:]} {
		raw, _ := json.Marshal(part)
		chunk(&b, `{"tool_calls":[{"index":0,"function":{"arguments":`+
			string(raw)+`}}]}`, "")
	}
	chunk(&b, `{}`, "tool_calls")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// fakeChatAPI scripts successive /chat/completions responses.
type fakeChatAPI struct {
	calls   atomic.Int32
	scripts []string
	// lastBody captures the final request for assertions.
	lastBody atomic.Pointer[[]byte]
}

func (f *fakeChatAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		body := buf.Bytes()
		f.lastBody.Store(&body)

		n := int(f.calls.Add(1)) - 1
		if n >= len(f.scripts) {
			http.Error(w, "unscripted call", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(f.scripts[n]))
	})
}

func startChatGateway(t *testing.T, fake *fakeChatAPI) *httptest.Server {
	t.Helper()
	djangoSrv := httptest.NewServer(fakeDjangoMux(t))
	t.Cleanup(djangoSrv.Close)
	modelSrv := httptest.NewServer(fake.handler())
	t.Cleanup(modelSrv.Close)
	rdb := startRedis(t)

	log := quietLogger()
	metrics := obs.NewMetrics()
	cfg := config.Config{
		DjangoBaseURL:     djangoSrv.URL + "/api/v1",
		DjangoPublicHost:  "api.example.test",
		MediaURLTemplate:  "https://assets.{domain}/x/{path}",
		TenantCacheTTL:    time.Minute,
		NegativeCacheTTL:  time.Minute,
		UpstreamTimeout:   5 * time.Second,
		RateLimitPerMin:   6000,
		RateLimitBurst:    1000,
		ChatAPIKey:        "sk-test-fake",
		ChatBaseURL:       modelSrv.URL,
		ChatModel:         "gemini-3.7-flash",
		ChatEffort:        "low",
		ChatMaxTokens:     1024,
		ChatMaxTurns:      10,
		ChatMaxIterations: 4,
		ChatRatePerMin:    600,
		ChatRateBurst:     100,
		ChatMaxMessageLen: 2000,
		ConversationTTL:   time.Hour,
	}
	dj := django.New(cfg.DjangoBaseURL, cfg.DjangoPublicHost, "test-secret",
		cfg.UpstreamTimeout, log, metrics)
	resolver := tenant.NewResolver(dj, rdb,
		cfg.TenantCacheTTL, cfg.NegativeCacheTTL, log, metrics)

	handler := server.New(server.Deps{
		Cfg: cfg, Log: log, Metrics: metrics, Redis: rdb,
		Django: dj, Resolver: resolver, Version: "test",
		ChatOpts: []option.RequestOption{
			option.WithBaseURL(modelSrv.URL),
		},
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

type sseEvent struct {
	name string
	data map[string]any
}

func postChat(t *testing.T, gwURL string, body map[string]any) []sseEvent {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(gwURL+"/chat", "application/json",
		bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream",
		resp.Header.Get("Content-Type"))

	var events []sseEvent
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	var current sseEvent
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			current = sseEvent{name: strings.TrimPrefix(line, "event: ")}
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			require.NoError(t, json.Unmarshal(
				[]byte(strings.TrimPrefix(line, "data: ")), &data))
			current.data = data
			events = append(events, current)
		}
	}
	require.NoError(t, scanner.Err())
	return events
}

func TestChatTextOnlyTurn(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		textTurnSSE("Γεια σου! ", "Πώς μπορώ να βοηθήσω;"),
	}}
	gw := startChatGateway(t, fake)

	events := postChat(t, gw.URL, map[string]any{"message": "γεια"})

	var text strings.Builder
	var done map[string]any
	for _, e := range events {
		switch e.name {
		case "delta":
			text.WriteString(e.data["text"].(string))
		case "done":
			done = e.data
		}
	}
	assert.Equal(t, "Γεια σου! Πώς μπορώ να βοηθήσω;", text.String())
	require.NotNil(t, done, "done event expected")
	assert.NotEmpty(t, done["conversationId"])
	assert.Equal(t, false, done["cartMutated"])

	// The system prompt must scope the assistant to the tenant's store,
	// and the configured model must reach the wire.
	body := *fake.lastBody.Load()
	assert.Contains(t, string(body), "Webside")
	assert.Contains(t, string(body), "gemini-3.7-flash")
}

func TestChatToolUseTurnMutatesCart(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
		textTurnSSE("Το πρόσθεσα στο καλάθι σου!"),
	}}
	gw := startChatGateway(t, fake)

	events := postChat(t, gw.URL, map[string]any{
		"message": "βάλε τον φορτιστή στο καλάθι",
	})

	var done map[string]any
	for _, e := range events {
		if e.name == "done" {
			done = e.data
		}
	}
	require.NotNil(t, done)
	assert.Equal(t, true, done["cartMutated"])
	assert.Equal(t, "29eb4495-e018-45e7-b59c-6646302bd4ef", done["cartId"])
	assert.Equal(t, int32(2), fake.calls.Load(),
		"tool result must be sent back for a second model call")

	// The follow-up request must contain the tool result from the real
	// in-process tool execution (cart total from the fixture) attributed
	// to the streamed tool-call id.
	body := *fake.lastBody.Load()
	assert.Contains(t, string(body), "call_e2e_1")
	assert.Contains(t, string(body), "929.36")
}

func TestChatConversationPersistsAcrossTurns(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		textTurnSSE("Πρώτη απάντηση."),
		textTurnSSE("Δεύτερη απάντηση."),
	}}
	gw := startChatGateway(t, fake)

	events := postChat(t, gw.URL, map[string]any{"message": "πρώτο"})
	var convID string
	for _, e := range events {
		if e.name == "done" {
			convID = e.data["conversationId"].(string)
		}
	}
	require.NotEmpty(t, convID)

	postChat(t, gw.URL, map[string]any{
		"message": "δεύτερο", "conversationId": convID,
	})

	// The second request must carry the first exchange as history.
	body := *fake.lastBody.Load()
	assert.Contains(t, string(body), "πρώτο")
	assert.Contains(t, string(body), "Πρώτη απάντηση.")
	assert.Contains(t, string(body), "δεύτερο")
}

//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store's (el) words for a refused request and for a turn that
// reached the shopper with no prose (internal/chat/messages.go).
const (
	elRefusal        = "Λυπάμαι, δεν μπορώ να βοηθήσω με αυτό το αίτημα."
	elTurnIncomplete = "Χρειάστηκα περισσότερα βήματα από όσα επιτρέπονται"
	elConversation   = "Αυτή η συνομιλία ολοκληρώθηκε"
)

// toolCall is one call of a scripted multi-call response. extra is
// written verbatim beside the function, empty for none.
type toolCall struct {
	id, name, args, extra string
}

// toolCallsTurnSSE is a streamed assistant turn that calls every tool
// in calls, whole, in one chunk.
func toolCallsTurnSSE(calls ...toolCall) string {
	parts := make([]string, 0, len(calls))
	for i, c := range calls {
		args, _ := json.Marshal(c.args)
		part := fmt.Sprintf(`{"index":%d,"id":%q,"type":"function",`+
			`"function":{"name":%q,"arguments":%s}`, i, c.id, c.name, args)
		if c.extra != "" {
			part += `,"extra_content":` + c.extra
		}
		parts = append(parts, part+"}")
	}
	var b strings.Builder
	chunk(&b, `{"role":"assistant","tool_calls":[`+
		strings.Join(parts, ",")+`]}`, "")
	chunk(&b, `{}`, "tool_calls")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// sentMessages decodes the messages of the model request at index n.
func sentMessages(t *testing.T, fake *fakeChatAPI, n int) []map[string]any {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Greater(t, len(fake.bodies), n)
	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(fake.bodies[n], &req))
	return req.Messages
}

// toolReplies maps each tool message's call id to its content.
func toolReplies(messages []map[string]any) map[string]string {
	out := map[string]string{}
	for _, m := range messages {
		if m["role"] == "tool" {
			content, _ := m["content"].(string)
			out[m["tool_call_id"].(string)] = content
		}
	}
	return out
}

func deltaText(events []sseEvent) string {
	var b strings.Builder
	for _, e := range events {
		if e.name == "delta" {
			b.WriteString(e.data["text"].(string))
		}
	}
	return b.String()
}

func toolEvents(events []sseEvent, status string) int {
	n := 0
	for _, e := range events {
		if e.name == "tool" && e.data["status"] == status {
			n++
		}
	}
	return n
}

// A model that refuses gets the store's own refusal, in the turn's
// language, never the provider's wording; the conversation keeps that
// refusal as the assistant's turn.
func TestChatModelRefusalIsTheStoresRefusal(t *testing.T) {
	var b strings.Builder
	chunk(&b, `{"role":"assistant","refusal":""}`, "")
	chunk(&b, `{"refusal":"I will not help with that."}`, "")
	chunk(&b, `{}`, "stop")
	b.WriteString("data: [DONE]\n\n")
	fake := &fakeChatAPI{scripts: []string{b.String()}}
	gw, rdb := newChatGateway(t, fake, fakeDjangoMux(t), quietLogger())

	events := postChat(t, gw.URL, map[string]any{"message": "κάτι"})

	text := deltaText(events)
	assert.True(t, strings.HasPrefix(text, elRefusal), text)
	assert.NotContains(t, text, "I will not")
	done := lastEvent(t, events, "done")
	conv := storedConversation(t, rdb, done["conversationId"].(string))
	require.NotNil(t, conv)
	turns := conv["turns"].([]any)
	require.Len(t, turns, 2)
	assert.Equal(t, text, turns[1].(map[string]any)["text"])
	assert.EqualValues(t, 1, fake.calls.Load())
}

// A response may run at most eight tool calls. Every call beyond them is
// still answered, with an error the model can act on, because a call id
// left without its tool message fails the next request.
func TestChatAnswersToolCallsBeyondTheLimitWithAnError(t *testing.T) {
	calls := make([]toolCall, 9)
	for i := range calls {
		calls[i] = toolCall{
			id: fmt.Sprintf("call_%d", i+1), name: "search_products",
			args: `{"query": "θήκη"}`,
		}
	}
	fake := &fakeChatAPI{scripts: []string{
		toolCallsTurnSSE(calls...), textTurnSSE("Βρήκα θήκες."),
	}}
	gw := startChatGateway(t, fake)

	events := postChat(t, gw.URL, map[string]any{"message": "θήκες"})

	assert.Equal(t, 8, toolEvents(events, "running"))
	assert.Equal(t, 8, toolEvents(events, "done"))
	replies := toolReplies(sentMessages(t, fake, 1))
	require.Len(t, replies, 9, "every call id has its tool message")
	for i := 1; i <= 8; i++ {
		assert.NotContains(t, replies[fmt.Sprintf("call_%d", i)], "ERROR")
	}
	assert.Equal(t,
		"ERROR: too many tool calls at once; make at most 8 per step",
		replies["call_9"])
	lastEvent(t, events, "done")
}

// Arguments that are not a JSON object never reach a tool: the model is
// told so in the call's tool message and the turn carries on.
func TestChatAnswersMalformedToolArgumentsWithAnError(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolCallsTurnSSE(toolCall{
			id: "call_bad", name: "search_products", args: `{"query": "θή`,
		}),
		textTurnSSE("Δοκιμάζω ξανά."),
	}}
	gw := startChatGateway(t, fake)

	events := postChat(t, gw.URL, map[string]any{"message": "θήκες"})

	assert.Zero(t, toolEvents(events, "running"), "no tool ran")
	assert.Equal(t, map[string]string{
		"call_bad": "ERROR: invalid tool arguments",
	}, toolReplies(sentMessages(t, fake, 1)))
	assert.Equal(t, "Δοκιμάζω ξανά.", deltaText(events))
	lastEvent(t, events, "done")
}

// A turn that reaches the shopper with no prose (an empty final message,
// or every iteration spent on tools) says so instead of an empty bubble,
// and the conversation keeps that answer.
func TestChatTurnWithoutProseSaysItIsIncomplete(t *testing.T) {
	search := toolUseTurnSSE("search_products", `{"query": "θήκη"}`)
	for name, scripts := range map[string][]string{
		"empty final message": {textTurnSSE()},
		"iterations spent":    {search, search, search, search},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeChatAPI{scripts: scripts}
			gw, rdb := newChatGateway(t, fake, fakeDjangoMux(t),
				quietLogger()) // 4 iterations

			events := postChat(t, gw.URL, map[string]any{"message": "θήκες"})

			text := deltaText(events)
			assert.True(t, strings.HasPrefix(text, elTurnIncomplete), text)
			done := lastEvent(t, events, "done")
			conv := storedConversation(t, rdb,
				done["conversationId"].(string))
			require.NotNil(t, conv)
			turns := conv["turns"].([]any)
			require.Len(t, turns, 2)
			assert.Equal(t, text, turns[1].(map[string]any)["text"])
		})
	}
}

// A conversation at its turn cap is a 409 in the store's language,
// before any model call.
func TestChatRefusesAFullConversation(t *testing.T) {
	fake := &fakeChatAPI{}
	gw, rdb := newChatGateway(t, fake, fakeDjangoMux(t), quietLogger())
	id := uuid.NewString()
	turns := make([]map[string]string, chatMaxTurns)
	for i := range turns {
		turns[i] = map[string]string{"role": "user", "text": "γεια"}
	}
	raw, err := json.Marshal(map[string]any{
		"id": id, "turns": turns, "createdAt": time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, rdb.Set(t.Context(), "ag:demostore:chat:"+id, raw,
		time.Hour).Err())

	body, err := json.Marshal(map[string]any{
		"message": "κι άλλο", "conversationId": id,
	})
	require.NoError(t, err)
	resp, err := http.Post(gw.URL+"/chat", "application/json",
		bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	var payload map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	assert.Contains(t, payload["error"], elConversation)
	assert.Zero(t, fake.calls.Load(), "no model call for a full conversation")
}

// A shopper who leaves mid-turn is no server fault: the turn is logged
// as abandoned, not failed, and a turn that changed no cart stores
// nothing.
func TestChatClientDisconnectMidTurnStoresNothing(t *testing.T) {
	asked := make(chan struct{})
	var once sync.Once
	fake := &fakeChatAPI{answer: func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(asked) })
		<-r.Context().Done()
	}}
	logs := &lockedBuffer{}
	gw, rdb := newChatGateway(t, fake, fakeDjangoMux(t),
		slog.New(slog.NewJSONHandler(logs, nil)))

	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	go func() {
		select {
		case <-asked:
			leave()
		case <-ctx.Done():
		}
	}()
	raw, err := json.Marshal(map[string]any{"message": "θήκες"})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gw.URL+"/chat", bytes.NewReader(raw))
	require.NoError(t, err)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(),
			`"msg":"chat turn abandoned by client"`)
	}, 5*time.Second, 20*time.Millisecond)
	assert.NotContains(t, logs.String(), `"msg":"chat turn failed"`)
	assert.NotContains(t, logs.String(), `"level":"ERROR"`)
	assert.Empty(t, storedConversations(t, rdb))
}

// Gemini attaches a thought signature to each tool call, and refuses the
// next request unless the assistant message carries it back on the same
// call. A call streamed without one is sent back without one.
func TestChatEchoesEachToolCallsThoughtSignature(t *testing.T) {
	const signature = `{"google":{"thought_signature":"c2lnLWJ5LWdlbWluaQ=="}}`
	fake := &fakeChatAPI{scripts: []string{
		toolCallsTurnSSE(
			toolCall{
				id: "call_plain", name: "search_products",
				args: `{"query": "θήκη"}`,
			},
			toolCall{
				id: "call_signed", name: "search_products",
				args: `{"query": "φορτιστής"}`, extra: signature,
			},
		),
		textTurnSSE("Βρήκα."),
	}}
	gw := startChatGateway(t, fake)

	postChat(t, gw.URL, map[string]any{"message": "θήκες"})

	var calls []any
	for _, m := range sentMessages(t, fake, 1) {
		if m["role"] == "assistant" {
			calls, _ = m["tool_calls"].([]any)
		}
	}
	require.Len(t, calls, 2)
	byID := map[string]map[string]any{}
	for _, c := range calls {
		call := c.(map[string]any)
		byID[call["id"].(string)] = call
	}
	assert.NotContains(t, byID["call_plain"], "extra_content")
	echoed, err := json.Marshal(byID["call_signed"]["extra_content"])
	require.NoError(t, err)
	assert.JSONEq(t, signature, string(echoed))
}

// The model credential is the store's own: every request a store's turn
// sends the provider carries that store's key, never another store's,
// with turns of two stores running at once on the one shared client.
func TestChatSendsEachStoresOwnModelKey(t *testing.T) {
	stores := map[string]struct{ name, key string }{
		"alpha.test": {"Alpha Store", "sk-alpha-key"},
		"acme.test":  {"Acme Store", "sk-acme-key"},
	}
	fixture, err := os.ReadFile(fixturePath("tenant_resolve_demostore.json"))
	require.NoError(t, err)
	base := fakeDjangoMux(t)
	djangoH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tenant/resolve" {
			base.ServeHTTP(w, r)
			return
		}
		domain := r.URL.Query().Get("domain")
		store, ok := stores[domain]
		if !ok {
			t.Errorf("resolve for an unexpected domain %q", domain)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		schema := strings.TrimSuffix(domain, ".test")
		var doc map[string]any
		if err := json.Unmarshal(fixture, &doc); err != nil {
			t.Error(err)
			return
		}
		doc["schemaName"], doc["name"] = schema, schema
		doc["storeName"], doc["chatApiKey"] = store.name, store.key
		doc["primaryDomain"] = domain
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})

	type sent struct{ auth, body string }
	var mu sync.Mutex
	var seen []sent
	model := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, sent{r.Header.Get("Authorization"), string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(textTurnSSE("Γεια!")))
	})
	gw := httptest.NewServer(chatGatewayHandler(t, startRedis(t), model,
		djangoH, quietLogger()))
	t.Cleanup(gw.Close)

	const turnsPerStore = 10
	var wg sync.WaitGroup
	for host := range stores {
		for range turnsPerStore {
			wg.Go(func() {
				raw, _ := json.Marshal(map[string]any{"message": "γεια"})
				req, err := http.NewRequestWithContext(t.Context(),
					http.MethodPost, gw.URL+"/chat", bytes.NewReader(raw))
				if !assert.NoError(t, err) {
					return
				}
				req.Host = host
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if !assert.NoError(t, err) {
					return
				}
				defer func() { _ = resp.Body.Close() }()
				assert.Equal(t, http.StatusOK, resp.StatusCode)
				_, _ = io.Copy(io.Discard, resp.Body)
			})
		}
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 2*turnsPerStore)
	perStore := map[string]int{}
	for _, s := range seen {
		var owner string
		for _, store := range stores {
			if strings.Contains(s.body, store.name) {
				owner = store.name
				assert.Equal(t, "Bearer "+store.key, s.auth,
					"a turn of %s sent another key", store.name)
			}
		}
		require.NotEmpty(t, owner, "a model request names no store")
		perStore[owner]++
	}
	assert.Equal(t, map[string]int{
		"Alpha Store": turnsPerStore, "Acme Store": turnsPerStore,
	}, perStore)
}

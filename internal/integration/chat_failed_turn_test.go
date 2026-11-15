//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
)

// activeCartLine is how the system prompt names the conversation's cart
// to the model.
const activeCartLine = "The shopper's active cartId is " + chatCartID

// noCartLine is the system prompt of a turn that has no cart.
const noCartLine = "The shopper has no active cart yet"

// answering is a model API that answers its calls in order: a script is
// streamed, and the call after the last script is answered by then,
// like every call after it.
func answering(then http.HandlerFunc, scripts ...string) *fakeChatAPI {
	fake := &fakeChatAPI{}
	fake.answer = func(w http.ResponseWriter, r *http.Request) {
		n := int(fake.calls.Load()) - 1
		if n < len(scripts) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(scripts[n]))
			return
		}
		then(w, r)
	}
	return fake
}

func modelFails(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "model down", http.StatusInternalServerError)
}

// storedConversation is the conversation as Redis holds it; nil when
// none is stored under id.
func storedConversation(
	t *testing.T, rdb *redis.Client, id string,
) map[string]any {
	t.Helper()
	raw, err := rdb.Get(context.Background(),
		"ag:demostore:chat:"+id).Result()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	require.NoError(t, err)
	var conv map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &conv))
	return conv
}

func storedConversations(t *testing.T, rdb *redis.Client) []string {
	t.Helper()
	keys, err := rdb.Keys(context.Background(),
		"ag:demostore:chat:????????-*").Result()
	require.NoError(t, err)
	return keys
}

// A visitor's turn adds a product, the store accepts it, and the next
// model call fails. The error event says what done would have said about
// the cart: it changed, and which cart to adopt. The conversation keeps
// the cart, and the event names the conversation, so the next turn is
// told to use that cart and creates no other.
func TestChatFailedTurnTellsTheWidgetWhatItDidToTheCart(t *testing.T) {
	for name, tc := range map[string]struct {
		then       http.HandlerFunc
		code       any
		retryAfter any
	}{
		"the provider answers an error": {then: modelFails},
		"the provider throttles": {
			then: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error": {"code": 429, ` +
					`"message": "Resource exhausted."}}`))
			},
			code: "rate_limited", retryAfter: float64(3),
		},
		"the provider fails inside the stream": {
			then: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(`data: {"error": {"code": 503, ` +
					`"message": "overloaded"}}` + "\n\n"))
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := answering(tc.then,
				toolUseTurnSSE("add_to_cart", `{"productId": 1}`))
			gw, rdb := newChatGateway(t, fake, fakeDjangoMux(t),
				quietLogger())

			events := postChat(t, gw.URL, map[string]any{
				"message": "βάλε τον φορτιστή στο καλάθι",
			})

			require.Equal(t, "error", events[len(events)-1].name)
			failure := events[len(events)-1].data
			assert.Equal(t, []any{"running", "done"}, toolStatuses(events))
			assert.Equal(t, true, failure["cartMutated"])
			assert.Equal(t, chatCartID, failure["cartId"])
			assert.Equal(t, false, failure["cartTokenRefused"])
			assert.Equal(t, tc.code, failure["code"])
			assert.Equal(t, tc.retryAfter, failure["retryAfter"])
			assert.NotEmpty(t, failure["message"])
			id, _ := failure["conversationId"].(string)
			require.NotEmpty(t, id, "the event names the conversation")
			stored := storedConversation(t, rdb, id)
			require.NotNil(t, stored, "the conversation is stored")
			assert.Equal(t, chatCartID, stored["cartId"])
			assert.Empty(t, stored["turns"],
				"a failed turn's messages are not stored")

			// A widget that reads none of this still continues on the
			// conversation it knows, without a cart of its own.
			postChat(t, gw.URL, map[string]any{
				"message": "και άλλον έναν", "conversationId": id,
			})
			assert.Contains(t, string(*fake.lastBody.Load()),
				activeCartLine)
		})
	}
}

// A signed-in shopper's turn that fails after a change the store
// accepted says the cart changed, and names no cart to adopt: the cart
// is theirs through their session. A token the store refused on the way
// is reported too, so the retry is sent with a new one.
func TestChatFailedSignedInTurnReportsTheCartChange(t *testing.T) {
	for name, tc := range map[string]struct {
		token   string
		refused bool
	}{
		"a live token":                         {liveCartToken, false},
		"a token revoked before the read-back": {revokingCartToken, true},
	} {
		t.Run(name, func(t *testing.T) {
			fake := answering(modelFails,
				toolUseTurnSSE("add_to_cart", `{"productId": 1}`))
			djangoH, _ := tokenDjango(t)
			gw, rdb := newChatGateway(t, fake, djangoH, quietLogger())

			events := postChat(t, gw.URL, map[string]any{
				"message": "βάλε τον φορτιστή", "cartToken": tc.token,
			})

			require.Equal(t, "error", events[len(events)-1].name)
			failure := events[len(events)-1].data
			assert.Equal(t, true, failure["cartMutated"])
			assert.Equal(t, tc.refused, failure["cartTokenRefused"])
			assert.NotContains(t, failure, "cartId",
				"a signed-in cart is the session's; no id to adopt")
			assert.NotContains(t, failure, "conversationId")
			assert.Empty(t, storedConversations(t, rdb),
				"a signed-in turn has no cart for a conversation to keep")
		})
	}
}

// A turn that fails before any cart tool ran says the cart did not
// change, and stores nothing.
func TestChatFailedTurnWithoutACartChangeSaysSo(t *testing.T) {
	fake := answering(modelFails)
	gw, rdb := newChatGateway(t, fake, fakeDjangoMux(t), quietLogger())

	events := postChat(t, gw.URL, map[string]any{"message": "γεια"})

	failure := lastEvent(t, events, "error")
	assert.Equal(t, false, failure["cartMutated"])
	assert.Equal(t, false, failure["cartTokenRefused"])
	assert.NotContains(t, failure, "cartId")
	assert.NotContains(t, failure, "conversationId")
	assert.Empty(t, storedConversations(t, rdb))
}

// A turn the pod's shutdown cuts after a change the store accepted ends
// with the same error event, and the conversation keeps the cart: the
// retry reaches another pod, which reads it from Redis.
func TestChatTurnCutByShutdownReportsAndKeepsTheCart(t *testing.T) {
	asked := make(chan struct{})
	var once sync.Once
	fake := answering(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(asked) })
		<-r.Context().Done()
	}, toolUseTurnSSE("add_to_cart", `{"productId": 1}`))
	rdb := startRedis(t)
	handler := chatGatewayHandler(t, rdb, fake.handler(),
		fakeDjangoMux(t), quietLogger())
	base, shutdown := context.WithCancelCause(context.Background())
	t.Cleanup(func() { shutdown(nil) })
	gw := httptest.NewUnstartedServer(handler)
	gw.Config.BaseContext = func(net.Listener) context.Context { return base }
	gw.Start()
	t.Cleanup(gw.Close)
	go func() {
		<-asked
		shutdown(httpmw.ErrShuttingDown)
	}()

	events := postChat(t, gw.URL, map[string]any{"message": "βάλε το"})

	require.Equal(t, "error", events[len(events)-1].name)
	failure := events[len(events)-1].data
	assert.Equal(t, true, failure["cartMutated"])
	assert.Equal(t, chatCartID, failure["cartId"])
	assert.NotContains(t, failure, "code")
	id, _ := failure["conversationId"].(string)
	require.NotEmpty(t, id)
	stored := storedConversation(t, rdb, id)
	require.NotNil(t, stored, "saved though the request's context ended")
	assert.Equal(t, chatCartID, stored["cartId"])
}

// The shopper leaves (or presses stop) after a change the store
// accepted: nobody reads the stream, and the conversation still keeps
// the cart, so their next message is told to use it.
func TestChatTurnAbandonedAfterACartChangeKeepsTheCart(t *testing.T) {
	asked := make(chan struct{})
	var once sync.Once
	var fake *fakeChatAPI
	fake = answering(func(w http.ResponseWriter, r *http.Request) {
		if fake.calls.Load() > 3 {
			modelFails(w, r)
			return
		}
		once.Do(func() { close(asked) })
		<-r.Context().Done()
	}, textTurnSSE("Γεια σου!"),
		toolUseTurnSSE("add_to_cart", `{"productId": 1}`))
	gw, rdb := newChatGateway(t, fake, fakeDjangoMux(t), quietLogger())
	first := doneOf(t, postChat(t, gw.URL, map[string]any{"message": "γεια"}))
	id := first["conversationId"].(string)
	require.NotContains(t, storedConversation(t, rdb, id), "cartId")

	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	go func() {
		<-asked
		leave()
	}()
	raw, err := json.Marshal(map[string]any{
		"message": "βάλε τον φορτιστή", "conversationId": id,
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gw.URL+"/chat", bytes.NewReader(raw))
	require.NoError(t, err)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	require.Eventually(t, func() bool {
		return storedConversation(t, rdb, id)["cartId"] == chatCartID
	}, 5*time.Second, 20*time.Millisecond,
		"the conversation keeps the cart the abandoned turn filled")
	assert.Len(t, storedConversation(t, rdb, id)["turns"], 2,
		"the abandoned turn's messages are not stored")

	postChat(t, gw.URL, map[string]any{
		"message": "τι έχει το καλάθι;", "conversationId": id,
	})
	assert.Contains(t, string(*fake.lastBody.Load()), activeCartLine)
}

// A signed-in shopper's cart is reached through their token alone, so a
// conversation keeps no cart of a signed-in turn: after a sign-out the
// same conversation goes on without a token, and it must not be told to
// use a cart the store serves to its owner only. The guest cart an
// earlier turn made is dropped too: a sign-in merges it away.
func TestChatConversationKeepsNoCartOfASignedInTurn(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1}`),
		textTurnSSE("Το πρόσθεσα."),
		toolUseTurnSSE("add_to_cart", `{"productId": 1}`),
		textTurnSSE("Και άλλο ένα."),
		textTurnSSE("Το καλάθι σου είναι άδειο."),
	}}
	djangoH, _ := tokenDjango(t)
	gw, rdb := newChatGateway(t, fake, djangoH, quietLogger())

	visitor := doneOf(t, postChat(t, gw.URL, map[string]any{
		"message": "βάλε τον φορτιστή",
	}))
	id := visitor["conversationId"].(string)
	require.Equal(t, chatCartID, storedConversation(t, rdb, id)["cartId"])

	signedIn := doneOf(t, postChat(t, gw.URL, map[string]any{
		"message": "και άλλον έναν", "conversationId": id,
		"cartToken": liveCartToken,
	}))
	assert.Equal(t, true, signedIn["cartMutated"])
	assert.NotContains(t, signedIn, "cartId")
	assert.NotContains(t, storedConversation(t, rdb, id), "cartId")

	signedOut := doneOf(t, postChat(t, gw.URL, map[string]any{
		"message": "τι έχει το καλάθι μου;", "conversationId": id,
	}))
	assert.NotContains(t, signedOut, "cartId")
	body := string(*fake.lastBody.Load())
	assert.NotContains(t, body, chatCartID)
	assert.Contains(t, body, noCartLine)
}

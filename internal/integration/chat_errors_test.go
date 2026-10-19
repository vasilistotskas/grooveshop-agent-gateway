//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
)

// cartItemDjango is the fake Django with POST /cart/item answered by add
// (nil: the fixture), recording each add's Idempotency-Key.
func cartItemDjango(
	t *testing.T, add http.HandlerFunc,
) (http.Handler, func() []string) {
	t.Helper()
	base := fakeDjangoMux(t)
	var mu sync.Mutex
	var keys []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/cart/item" {
			mu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			mu.Unlock()
			if add != nil {
				add(w, r)
				return
			}
		}
		base.ServeHTTP(w, r)
	})
	return h, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), keys...)
	}
}

// A refused tool call is a failed step for the widget, and the model is
// told why in the store's own code and words.
func TestChatFailedToolIsAnErrorStepWithTheStoresReason(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 5}`),
		textTurnSSE("Υπάρχουν μόνο 2 διαθέσιμα."),
	}}
	djangoH, _ := cartItemDjango(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail": "Μόνο 2 διαθέσιμα.",` +
			` "code": "insufficient_stock"}`))
	})
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

	events := postChat(t, gw.URL, map[string]any{"message": "βάλε 5"})

	var statuses []any
	for _, e := range events {
		if e.name == "tool" {
			statuses = append(statuses, e.data["status"])
		}
	}
	assert.Equal(t, []any{"running", "error"}, statuses)
	body := string(*fake.lastBody.Load())
	assert.Contains(t, body, "insufficient_stock")
	assert.Contains(t, body, "Μόνο 2 διαθέσιμα.")
}

// The widget sends a retried message with its turn id again: the retry's
// add carries the first attempt's key, so Django answers it with the
// first outcome instead of adding the product twice. Another turn's add
// is another change.
func TestChatRetriedTurnRepeatsItsIdempotencyKey(t *testing.T) {
	addTurn := func() []string {
		return []string{
			toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
			textTurnSSE("Έγινε."),
		}
	}
	var scripts []string
	for range 3 {
		scripts = append(scripts, addTurn()...)
	}
	fake := &fakeChatAPI{scripts: scripts}
	djangoH, keys := cartItemDjango(t, nil)
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())
	const turn = "2f7d9c1a-5b3e-4e8f-a6d2-9c0b1e4f7a35"
	cart := "29eb4495-e018-45e7-b59c-6646302bd4ef"

	postChat(t, gw.URL, map[string]any{
		"message": "βάλε το", "cartId": cart, "turnId": turn})
	postChat(t, gw.URL, map[string]any{
		"message": "βάλε το", "cartId": cart, "turnId": turn})
	postChat(t, gw.URL, map[string]any{
		"message": "βάλε το", "cartId": cart,
		"turnId": "6a1e3b7c-0d4f-4a2b-8c9e-1f5d7b3a9c20"})

	got := keys()
	require.Len(t, got, 3)
	assert.NotEmpty(t, got[0])
	assert.Equal(t, got[0], got[1])
	assert.NotEqual(t, got[0], got[2])
}

func TestChatRefusesAMalformedTurnID(t *testing.T) {
	gw := startChatGateway(t, &fakeChatAPI{})

	status, _ := postChatFrom(t, gw.URL, "192.0.2.9", map[string]any{
		"message": "γεια", "turnId": "not a uuid"})

	assert.Equal(t, http.StatusBadRequest, status)
}

// A turn the quota refuses says when the full window resets.
func TestChatQuotaRefusalSaysWhenToRetry(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{textTurnSSE("ένα")}}
	gw := startChatGateway(t, fake, func(c *config.Config) {
		c.ChatVisitorTurnsPerDay = 1
	})
	post := func() *http.Response {
		raw, _ := json.Marshal(map[string]any{"message": "γεια"})
		req, err := http.NewRequest(http.MethodPost, gw.URL+"/chat",
			bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", "198.51.100.40")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	require.Equal(t, http.StatusOK, post().StatusCode)
	refused := post()

	assert.Equal(t, http.StatusTooManyRequests, refused.StatusCode)
	wait, err := strconv.Atoi(refused.Header.Get("Retry-After"))
	require.NoError(t, err)
	assert.Positive(t, wait)
	assert.LessOrEqual(t, wait, 24*60*60, "the visitor's window is a day")
}

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
// (nil: the fixture), told how many adds came before, and a count of the
// adds it was sent.
func cartItemDjango(
	t *testing.T, add func(w http.ResponseWriter, before int),
) (http.Handler, func() int) {
	t.Helper()
	base := fakeDjangoMux(t)
	var mu sync.Mutex
	adds := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/cart/item" {
			mu.Lock()
			before := adds
			adds++
			mu.Unlock()
			if add != nil {
				add(w, before)
				return
			}
		}
		base.ServeHTTP(w, r)
	})
	return h, func() int {
		mu.Lock()
		defer mu.Unlock()
		return adds
	}
}

const chatCartID = "29eb4495-e018-45e7-b59c-6646302bd4ef"

func toolStatuses(events []sseEvent) []any {
	var statuses []any
	for _, e := range events {
		if e.name == "tool" {
			statuses = append(statuses, e.data["status"])
		}
	}
	return statuses
}

func lastEvent(t *testing.T, events []sseEvent, name string) map[string]any {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].name == name {
			return events[i].data
		}
	}
	require.Failf(t, "no event", "no %q event in %v", name, events)
	return nil
}

func storeRefusal(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_, _ = w.Write([]byte(`{"detail": "Μόνο 2 διαθέσιμα.",` +
		` "code": "insufficient_stock"}`))
}

// A refused tool call is a failed step for the widget, and the model is
// told why in the store's own code and words.
func TestChatFailedToolIsAnErrorStepWithTheStoresReason(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 5}`),
		textTurnSSE("Υπάρχουν μόνο 2 διαθέσιμα."),
	}}
	djangoH, _ := cartItemDjango(t, func(w http.ResponseWriter, _ int) {
		storeRefusal(w)
	})
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

	events := postChat(t, gw.URL, map[string]any{"message": "βάλε 5"})

	assert.Equal(t, []any{"running", "error"}, toolStatuses(events))
	body := string(*fake.lastBody.Load())
	assert.Contains(t, body, "insufficient_stock")
	assert.Contains(t, body, "Μόνο 2 διαθέσιμα.")
}

// The shopper retries a turn that added the product and then failed.
// The retry reaches the other pod, which finds the add recorded in the
// shared Redis: the product is added once, and the widget still learns
// that the cart changed.
func TestChatRetriedTurnOnAnotherPodDoesNotAddTwice(t *testing.T) {
	addThenFail := &fakeChatAPI{scripts: []string{
		// The next model call is unscripted: it fails the turn.
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
	}}
	addThenAnswer := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
		textTurnSSE("Το πρόσθεσα."),
	}}
	djangoH, adds := cartItemDjango(t, nil)
	rdb := startRedis(t)
	podA := newChatGatewayOn(t, rdb, addThenFail, djangoH, quietLogger())
	podB := newChatGatewayOn(t, rdb, addThenAnswer, djangoH, quietLogger())
	turn := map[string]any{"message": "βάλε το", "cartId": chatCartID,
		"turnId": "2f7d9c1a-5b3e-4e8f-a6d2-9c0b1e4f7a35"}

	failed := postChat(t, podA.URL, turn)
	require.NotNil(t, lastEvent(t, failed, "error"))
	require.Equal(t, 1, adds())

	retried := postChat(t, podB.URL, turn)

	assert.Equal(t, 1, adds(), "the retry must not add the product again")
	assert.Equal(t, []any{"running", "done"}, toolStatuses(retried))
	assert.Equal(t, true, lastEvent(t, retried, "done")["cartMutated"])
}

// Another message is another turn: the same add is made again.
func TestChatAnotherTurnAddsAgain(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
		textTurnSSE("Έγινε."),
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
		textTurnSSE("Έγινε."),
	}}
	djangoH, adds := cartItemDjango(t, nil)
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

	for _, id := range []string{"2f7d9c1a-5b3e-4e8f-a6d2-9c0b1e4f7a35",
		"6a1e3b7c-0d4f-4a2b-8c9e-1f5d7b3a9c20"} {
		postChat(t, gw.URL, map[string]any{
			"message": "βάλε κι άλλο", "cartId": chatCartID, "turnId": id})
	}

	assert.Equal(t, 2, adds())
}

// Within a turn, an add the store refused can be made again, and an
// identical add after a success is a new one.
func TestChatTurnRepeatsAnAddTheStoreRefused(t *testing.T) {
	add := toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`)
	fake := &fakeChatAPI{scripts: []string{add, add, add,
		textTurnSSE("Δύο στο καλάθι.")}}
	djangoH, adds := cartItemDjango(t,
		func(w http.ResponseWriter, before int) {
			if before == 0 {
				storeRefusal(w)
				return
			}
			serveFixture(t, w, "cart_item_created.json")
		})
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

	events := postChat(t, gw.URL, map[string]any{
		"message": "βάλε δύο", "cartId": chatCartID})

	assert.Equal(t, 3, adds())
	assert.Equal(t, []any{"running", "error", "running", "done",
		"running", "done"}, toolStatuses(events))
}

// An add the store did not confirm may have been made: the model is told
// to check the cart, and a repeat is refused before it reaches the store.
func TestChatTurnDoesNotRepeatAnUnconfirmedAdd(t *testing.T) {
	add := toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`)
	fake := &fakeChatAPI{scripts: []string{add, add,
		textTurnSSE("Έλεγξε το καλάθι σου.")}}
	djangoH, adds := cartItemDjango(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusBadGateway)
	})
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

	events := postChat(t, gw.URL, map[string]any{
		"message": "βάλε το", "cartId": chatCartID})

	assert.Equal(t, 1, adds())
	assert.Equal(t, []any{"running", "error", "running", "error"},
		toolStatuses(events))
	require.Len(t, fake.bodies, 3)
	assert.Contains(t, string(fake.bodies[1]), "get_cart to check")
	assert.Contains(t, string(fake.bodies[2]), "in_progress")
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

//go:build integration

package integration

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// liveCartToken is accepted by the fake Django below; any other
	// token is refused the way Django refuses an expired one.
	liveCartToken    = "Zk3n_Q8vX2-rT5yLmW9aB4cD7eF0gH1iJ6kN8pS3uVw"
	revokedCartToken = "Rv0k3d_Q8vX2-rT5yLmW9aB4cD7eF0gH1iJ6kN8pS3"
	// revokingCartToken is accepted for changes and refused on reads: a
	// token revoked between the change and its read-back.
	revokingCartToken = "Rv0k1ng_Q8vX2-rT5yLmW9aB4cD7eF0gH1iJ6kN8pS"
)

// lockedBuffer is a log sink the gateway's goroutines write while the
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// djangoCall is one request the fake Django received.
type djangoCall struct {
	route     string
	cartID    string
	cartToken string
}

// tokenDjango wraps the fixture Django: a cart call with the live token
// is served as usual, any other token gets Django's 401, and a token on
// a route that does not accept it fails the test.
func tokenDjango(t *testing.T) (http.Handler, func() []djangoCall) {
	t.Helper()
	inner := fakeDjangoMux(t)
	var mu sync.Mutex
	var calls []djangoCall
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		token := r.Header.Get("X-Cart-Token")
		mu.Lock()
		calls = append(calls, djangoCall{
			route: route, cartID: r.Header.Get("X-Cart-Id"),
			cartToken: token,
		})
		mu.Unlock()
		if token != "" {
			if !strings.HasPrefix(r.URL.Path, "/api/v1/cart") ||
				strings.Contains(r.URL.Path, "reserve-stock") {
				t.Errorf("cart token sent to %s", route)
			}
			accepted := token == liveCartToken ||
				(token == revokingCartToken && r.Method != http.MethodGet)
			if !accepted {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(
					`{"detail": "The cart token is invalid or has expired."}`))
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
	return h, func() []djangoCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]djangoCall(nil), calls...)
	}
}

func doneOf(t *testing.T, events []sseEvent) map[string]any {
	t.Helper()
	for _, e := range events {
		if e.name == "done" {
			return e.data
		}
	}
	require.FailNow(t, "done event expected")
	return nil
}

// assertTokenNowhere checks every place a turn leaves data: the model's
// requests, the stored conversation and the logs.
func assertTokenNowhere(
	t *testing.T, token string, fake *fakeChatAPI, logs *lockedBuffer,
	rdbKeys map[string]string,
) {
	t.Helper()
	fake.mu.Lock()
	for i, body := range fake.bodies {
		assert.NotContains(t, string(body), token, "model request %d", i)
	}
	fake.mu.Unlock()
	for k, v := range rdbKeys {
		assert.NotContains(t, k+v, token, "redis key %s", k)
	}
	assert.NotEmpty(t, logs.String(), "the turn logs at debug level")
	assert.NotContains(t, logs.String(), token)
}

// A signed-in shopper's turn acts on their cart through the token: it is
// sent instead of X-Cart-Id on the cart routes only, the model's cartId
// is ignored, and the token is in no model request, stored conversation
// or log line, a failed turn's debug dump included.
func TestChatActsOnTheSignedInCartThroughTheToken(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1,`+
			` "cartId": "6f1c1f3e-2d6a-4b0f-9a3e-2f6d6c1b9a10"}`),
		textTurnSSE("Το πρόσθεσα στο καλάθι σου!"),
		// The next turn has no script: the model API fails, and the
		// gateway logs the failed request at debug level.
	}}
	logs := &lockedBuffer{}
	djangoH, calls := tokenDjango(t)
	gw, rdb := newChatGateway(t, fake, djangoH, slog.New(
		slog.NewJSONHandler(logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))

	done := doneOf(t, postChat(t, gw.URL, map[string]any{
		"message": "βάλε τον φορτιστή στο καλάθι", "cartToken": liveCartToken,
	}))
	assert.Equal(t, true, done["cartMutated"])
	assert.Equal(t, false, done["cartTokenRefused"])
	events := postChat(t, gw.URL, map[string]any{
		"message": "και άλλον έναν", "cartToken": liveCartToken,
		"conversationId": done["conversationId"],
	})
	assert.Equal(t, "error", events[len(events)-1].name)

	var cartCalls int
	for _, c := range calls() {
		if !strings.HasPrefix(c.route, "GET /api/v1/cart") &&
			!strings.HasPrefix(c.route, "POST /api/v1/cart") {
			continue
		}
		cartCalls++
		assert.Equal(t, liveCartToken, c.cartToken, c.route)
		assert.Empty(t, c.cartID, c.route)
	}
	assert.Equal(t, 2, cartCalls, "the add and its read-back")

	stored := map[string]string{}
	keys, err := rdb.Keys(context.Background(), "*").Result()
	require.NoError(t, err)
	for _, k := range keys {
		if v, err := rdb.Get(context.Background(), k).Result(); err == nil {
			stored[k] = v
		}
	}
	require.NotEmpty(t, stored)
	assertTokenNowhere(t, liveCartToken, fake, logs, stored)
}

// Django refusing the token (expired, revoked) fails the cart tool, tells
// the model the cart could not be reached, and tells the widget to mint
// a new token, without claiming a cart change.
func TestChatSignalsARefusedCartToken(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1}`),
		textTurnSSE("Δεν μπόρεσα να φτάσω στο καλάθι σου."),
	}}
	djangoH, calls := tokenDjango(t)
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

	done := doneOf(t, postChat(t, gw.URL, map[string]any{
		"message": "βάλε τον φορτιστή στο καλάθι", "cartToken": revokedCartToken,
	}))

	assert.Equal(t, true, done["cartTokenRefused"])
	assert.Equal(t, false, done["cartMutated"])
	assert.Nil(t, done["cartId"])
	body := string(*fake.lastBody.Load())
	assert.Contains(t, body, "could not be reached")
	for _, c := range calls() {
		assert.Empty(t, c.cartID, c.route)
	}
}

// A token revoked after the change but before its read-back: the change
// happened, so the widget is told the cart changed and to mint a new
// token, and the model that the change must not be repeated.
func TestChatReportsAChangeWhoseReadBackWasRefused(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1}`),
		textTurnSSE("Το πρόσθεσα, έλεγξε το καλάθι σου."),
	}}
	djangoH, _ := tokenDjango(t)
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

	done := doneOf(t, postChat(t, gw.URL, map[string]any{
		"message": "βάλε τον φορτιστή", "cartToken": revokingCartToken,
	}))

	assert.Equal(t, true, done["cartMutated"])
	assert.Equal(t, true, done["cartTokenRefused"])
	body := string(*fake.lastBody.Load())
	assert.Contains(t, body, "do not repeat the change")
}

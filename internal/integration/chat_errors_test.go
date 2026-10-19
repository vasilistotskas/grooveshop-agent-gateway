//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
)

const chatCartID = "29eb4495-e018-45e7-b59c-6646302bd4ef"

// edgeSecret is the X-Origin-Verify value our Cloudflare zones stamp.
const edgeSecret = "edge-proof-secret"

// visitor is the shopper's address as Cloudflare connected it.
const visitor = "203.0.113.7"

// throughTheEdge is how the visitor's request reaches the gateway
// through our zone: Cloudflare's address for them, the proof, and the
// X-Forwarded-For chain ending in the edge node.
func throughTheEdge() http.Header {
	return http.Header{
		"Cf-Connecting-Ip": {visitor},
		"X-Origin-Verify":  {edgeSecret},
		"X-Forwarded-For":  {visitor + ", 172.70.1.1"},
	}
}

// idempotentDjango stands in for Django's IdempotencyMiddleware
// (core/middleware/idempotency.py) with the caller Django keys it on
// (core/client_ip.py): the visitor a request names beside the edge
// proof, else its last X-Forwarded-For hop, else the address it came
// from, which for a gateway call is the pod. An Idempotency-Key that
// caller already used is answered without adding again.
type idempotentDjango struct {
	mu   sync.Mutex
	used map[string]bool
	adds int
}

// pod is the fake Django one gateway pod calls.
func (d *idempotentDjango) pod(t *testing.T, name string) http.Handler {
	t.Helper()
	base := fakeDjangoMux(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/cart/item" {
			caller := name
			if hops := strings.Split(r.Header.Get("X-Forwarded-For"), ","); hops[0] != "" {
				caller = strings.TrimSpace(hops[len(hops)-1])
			}
			if r.Header.Get("X-Origin-Verify") == edgeSecret {
				caller = r.Header.Get("X-Real-IP")
			}
			key := r.Header.Get("Idempotency-Key")
			d.mu.Lock()
			if key == "" || !d.used[caller+"|"+key] {
				d.adds++
			}
			if key != "" {
				d.used[caller+"|"+key] = true
			}
			d.mu.Unlock()
		}
		base.ServeHTTP(w, r)
	})
}

func (d *idempotentDjango) added() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.adds
}

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

// A refused tool call is a failed step for the widget, and the model is
// told why in the store's own code and words.
func TestChatFailedToolIsAnErrorStepWithTheStoresReason(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 5}`),
		textTurnSSE("Υπάρχουν μόνο 2 διαθέσιμα."),
	}}
	base := fakeDjangoMux(t)
	djangoH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/cart/item" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail": "Μόνο 2 διαθέσιμα.",` +
				` "code": "insufficient_stock"}`))
			return
		}
		base.ServeHTTP(w, r)
	})
	gw, _ := newChatGateway(t, fake, djangoH, quietLogger())

	events := postChat(t, gw.URL, map[string]any{"message": "βάλε 5"})

	assert.Equal(t, []any{"running", "error"}, toolStatuses(events))
	body := string(*fake.lastBody.Load())
	assert.Contains(t, body, "insufficient_stock")
	assert.Contains(t, body, "Μόνο 2 διαθέσιμα.")
}

// retriedOnAnotherPod runs a turn that adds the product and then fails
// on pod A, and its retry, delivered with retryEdge, on pod B. It
// returns the retry's events and the adds Django made.
func retriedOnAnotherPod(
	t *testing.T, retryEdge http.Header,
) ([]sseEvent, int) {
	t.Helper()
	addThenFail := &fakeChatAPI{scripts: []string{
		// The next model call is unscripted: it fails the turn.
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
	}}
	addThenAnswer := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
		textTurnSSE("Το πρόσθεσα."),
	}}
	django := &idempotentDjango{used: map[string]bool{}}
	rdb := startRedis(t)
	podA := newChatGatewayOn(t, rdb, addThenFail, django.pod(t, "pod-a"),
		quietLogger())
	podB := newChatGatewayOn(t, rdb, addThenAnswer, django.pod(t, "pod-b"),
		quietLogger())
	turn := map[string]any{"message": "βάλε το", "cartId": chatCartID,
		"turnId": "2f7d9c1a-5b3e-4e8f-a6d2-9c0b1e4f7a35"}

	failed := postChatAs(t, podA.URL, throughTheEdge(), turn)
	require.NotNil(t, lastEvent(t, failed, "error"))
	require.Equal(t, 1, django.added())

	return postChatAs(t, podB.URL, retryEdge, turn), django.added()
}

// The shopper retries a turn that added the product and then failed,
// and the retry reaches the other pod. Both pods name the same visitor
// to Django and send the turn's key, so Django answers the retry with
// the first add: the product is added once, and the widget still learns
// that the cart changed.
func TestChatRetriedTurnOnAnotherPodAddsOnce(t *testing.T) {
	retried, adds := retriedOnAnotherPod(t, throughTheEdge())

	assert.Equal(t, 1, adds, "the retry must not add the product again")
	assert.Equal(t, []any{"running", "done"}, toolStatuses(retried))
	assert.Equal(t, true, lastEvent(t, retried, "done")["cartMutated"])
}

// An address the visitor writes themselves is not the one the gateway
// names: the retry is still the same caller to Django.
func TestChatRetryWithASpoofedAddressIsTheSameCaller(t *testing.T) {
	spoofed := throughTheEdge()
	spoofed.Set("X-Real-Ip", "198.51.100.66")

	_, adds := retriedOnAnotherPod(t, spoofed)

	assert.Equal(t, 1, adds)
}

// Another message is another turn: the same add is made again.
func TestChatAnotherTurnAddsAgain(t *testing.T) {
	fake := &fakeChatAPI{scripts: []string{
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
		textTurnSSE("Έγινε."),
		toolUseTurnSSE("add_to_cart", `{"productId": 1, "quantity": 1}`),
		textTurnSSE("Έγινε."),
	}}
	django := &idempotentDjango{used: map[string]bool{}}
	gw, _ := newChatGateway(t, fake, django.pod(t, "pod-a"), quietLogger())

	for _, id := range []string{"2f7d9c1a-5b3e-4e8f-a6d2-9c0b1e4f7a35",
		"6a1e3b7c-0d4f-4a2b-8c9e-1f5d7b3a9c20"} {
		postChatAs(t, gw.URL, throughTheEdge(), map[string]any{
			"message": "βάλε κι άλλο", "cartId": chatCartID, "turnId": id})
	}

	assert.Equal(t, 2, django.added())
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

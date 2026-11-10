//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// completionSurface is one protocol's way to a cash-on-delivery checkout
// that is ready to complete, and to completing it.
type completionSurface struct {
	name string
	// ready returns the id of a checkout that is ready to complete.
	ready func(t *testing.T) string
	// complete asks for the order under key and returns the status the
	// surface rendered, "" when it answered with an error or not at all.
	complete func(ctx context.Context, id, key string) string
	// keyName is where the surface keeps that key's state in Redis.
	keyName func(id, key string) string
}

func completionSurfaces(t *testing.T, stack ucpStack) []completionSurface {
	t.Helper()
	base := stack.gw.URL + "/acp/checkout_sessions"
	acp := completionSurface{
		name: "acp",
		ready: func(t *testing.T) string {
			t.Helper()
			resp, created := acpCall(t, http.MethodPost, base, map[string]any{
				"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
				"currency":     "EUR",
				"capabilities": map[string]any{},
				"buyer": map[string]any{
					"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
					"email":        "maria@example.test",
					"phone_number": "+306912345678",
				},
				"fulfillment_details": map[string]any{
					"name": "Μαρία Παπαδοπούλου",
					"address": map[string]any{
						"name":     "Μαρία Παπαδοπούλου",
						"line_one": "Πανεπιστημίου 12", "city": "Αθήνα",
						"state": "GR-14", "country": "GR",
						"postal_code": "10431",
					},
				},
			}, uuid.NewString())
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			id := created["id"].(string)
			resp, updated := acpCall(t, http.MethodPost, base+"/"+id,
				map[string]any{
					"selected_fulfillment_options": []map[string]any{{
						"type": "shipping", "option_id": "acs:home_delivery",
						"item_ids": []string{"410"},
					}},
				}, uuid.NewString())
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "ready_for_payment", updated["status"])
			return id
		},
		complete: func(ctx context.Context, id, key string) string {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				base+"/"+id+"/complete",
				bytes.NewReader([]byte(`{"payment_data": {}}`)))
			if err != nil {
				return ""
			}
			req.Header.Set("Authorization", "Bearer "+acpBearerToken)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return ""
			}
			defer func() { _ = resp.Body.Close() }()
			var out map[string]any
			if resp.StatusCode != http.StatusOK ||
				json.NewDecoder(resp.Body).Decode(&out) != nil {
				return ""
			}
			status, _ := out["status"].(string)
			return status
		},
		keyName: func(id, key string) string {
			return "ag:demostore:acp:idem:complete:" + id + ":" + key
		},
	}

	session := connectMCP(t, stack.gw.URL)
	// Only from this protocol on does a tool's context end with its
	// request: on an older one a hang-up would test nothing.
	require.GreaterOrEqual(t,
		session.InitializeResult().ProtocolVersion, "2026-07-28")
	platform := httptest.NewServer(newWebhookSink().handler())
	t.Cleanup(platform.Close)
	profile := servePlatformProfile(t, platform.URL+"/ucp/orders", nil)
	meta := func(idem string) map[string]any {
		m := map[string]any{"ucp-agent": map[string]any{"profile": profile}}
		if idem != "" {
			m["idempotency-key"] = idem
		}
		return m
	}
	cod := map[string]any{"instruments": []any{map[string]any{
		"handler_id": ucp.HandlerID,
		"type":       ucp.InstrumentCashOnDelivery, "selected": true,
	}}}
	mcpSurface := completionSurface{
		name: "ucp",
		ready: func(t *testing.T) string {
			t.Helper()
			res := callTool(t, session, "create_checkout", map[string]any{
				"meta": meta(""), "cart_id": fixtureCartID,
				"checkout": map[string]any{
					"buyer": map[string]any{
						"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
						"email":        "maria@example.test",
						"phone_number": "+306912345678",
					},
					"fulfillment": map[string]any{
						"kind": "home_delivery", "providerCode": "acs",
						"countryCode": "GR", "city": "Αθήνα",
						"zipcode": "10431", "street": "Πανεπιστημίου",
						"streetNumber": "12", "regionId": "GR-14",
					},
					"payment": cod,
				},
			})
			require.False(t, res.IsError, "%v", res.Content)
			created := structured(t, res)
			require.Equal(t, "ready_for_complete", created["status"])
			return created["id"].(string)
		},
		complete: func(ctx context.Context, id, key string) string {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name: "complete_checkout",
				Arguments: map[string]any{
					"meta": meta(key), "id": id,
					"checkout": map[string]any{"payment": cod},
				},
			})
			if err != nil || res.IsError {
				return ""
			}
			raw, err := json.Marshal(res.StructuredContent)
			if err != nil {
				return ""
			}
			var out map[string]any
			if json.Unmarshal(raw, &out) != nil {
				return ""
			}
			status, _ := out["status"].(string)
			return status
		},
		keyName: func(id, key string) string {
			return "ag:demostore:idem:" + id + ":" + key
		},
	}
	return []completionSurface{acp, mcpSurface}
}

// The states a completion leaves an idempotency key in.
const (
	keyReleased = "released"
	keyInFlight = "inflight"
	keySpent    = "spent"
)

// bookkeeping is what a completion leaves in Redis.
type bookkeeping struct {
	// Session is the stored checkout's status.
	Session checkout.Status
	// Linked reports whether the order link names this checkout.
	Linked bool
	// Key is the idempotency key's state.
	Key string
}

func (stack ucpStack) bookkeeping(
	t *testing.T, id, keyName string,
) bookkeeping {
	t.Helper()
	ctx := context.Background()
	store := checkout.NewStore(stack.rdb)
	s, err := store.Load(ctx, "demostore", id)
	require.NoError(t, err)
	out := bookkeeping{Session: s.Status, Key: keySpent}

	link, err := store.OrderLinkFor(ctx, "demostore", fixtureOrderUUID)
	if !errors.Is(err, checkout.ErrNotFound) {
		require.NoError(t, err)
		out.Linked = link.CheckoutID == id
	}
	state, err := stack.rdb.Get(ctx, keyName).Result()
	switch {
	case errors.Is(err, redis.Nil):
		out.Key = keyReleased
	case err != nil:
		require.NoError(t, err)
	case state == keyInFlight:
		out.Key = keyInFlight
	}
	return out
}

// UCP's completion claim and ACP's body-hash record are different
// stores, but an outcome means the same for both: the same session, the
// same order link, the key spent or released.
func TestCompleteBookkeepingIsTheSameOnBothSurfaces(t *testing.T) {
	stack := startUCPGateway(t)
	surfaces := completionSurfaces(t, stack)
	outcomes := []struct {
		name  string
		fault *atomic.Bool
		want  bookkeeping
	}{
		{"an order placed", nil, bookkeeping{
			Session: checkout.StatusCompleted, Linked: true, Key: keySpent,
		}},
		{"an order the store did not confirm", &stack.faults.orderCreate,
			bookkeeping{
				Session: checkout.StatusCompleteInProgress, Key: keySpent,
			}},
		{"an order the store refused", &stack.faults.orderRefuse,
			bookkeeping{
				Session: checkout.StatusReadyForComplete, Key: keyReleased,
			}},
	}
	for _, outcome := range outcomes {
		for _, surface := range surfaces {
			t.Run(outcome.name+" over "+surface.name, func(t *testing.T) {
				id := surface.ready(t)
				if outcome.fault != nil {
					outcome.fault.Store(true)
					t.Cleanup(func() { outcome.fault.Store(false) })
				}
				key := uuid.NewString()
				surface.complete(context.Background(), id, key)

				assert.Equal(t, outcome.want,
					stack.bookkeeping(t, id, surface.keyName(id, key)))
			})
		}
	}
}

// requestWatch remembers the context of the last completion request the
// gateway was handed, so a test can tell when the gateway has seen its
// caller leave.
type requestWatch struct {
	mu  sync.Mutex
	ctx context.Context
}

func (w *requestWatch) front(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && (r.URL.Path == "/mcp" ||
			strings.HasSuffix(r.URL.Path, "/complete")) {
			w.mu.Lock()
			w.ctx = r.Context()
			w.mu.Unlock()
		}
		next.ServeHTTP(rw, r)
	})
}

func (w *requestWatch) last() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ctx
}

// A platform that gives up while Django is creating the order has still
// ordered. The gateway finishes what it started: the checkout is saved as
// placed, the order is linked and the key is spent, so the platform's
// retry is answered with the order instead of "in flight" until the
// session expires.
func TestCompleteOutlivesAPlatformThatHangsUp(t *testing.T) {
	watch := new(requestWatch)
	stack := startUCPGatewayBehind(t, watch.front)
	for _, surface := range completionSurfaces(t, stack) {
		t.Run(surface.name, func(t *testing.T) {
			id := surface.ready(t)
			before := stack.faults.orderCreates.Load()
			reached, answer := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(answer) })
			defer release()
			hold := func(*http.Request) {
				close(reached)
				<-answer
			}
			stack.faults.orderHold.Store(&hold)

			key := uuid.NewString()
			ctx, hangUp := context.WithCancel(context.Background())
			defer hangUp()
			gone := make(chan struct{})
			go func() {
				defer close(gone)
				surface.complete(ctx, id, key)
			}()

			select {
			case <-reached:
			case <-time.After(15 * time.Second):
				t.Fatal("the order was never sent to the store")
			}
			stack.faults.orderHold.Store(nil)
			served := watch.last()
			hangUp()
			select {
			case <-served.Done():
			case <-time.After(15 * time.Second):
				t.Fatal("the gateway never saw the platform leave")
			}
			// Only now does the store answer: the order exists.
			release()
			<-gone

			want := bookkeeping{
				Session: checkout.StatusCompleted, Linked: true, Key: keySpent,
			}
			keyName := surface.keyName(id, key)
			got := stack.bookkeeping(t, id, keyName)
			for deadline := time.Now().Add(10 * time.Second); got != want &&
				time.Now().Before(deadline); {
				time.Sleep(50 * time.Millisecond)
				got = stack.bookkeeping(t, id, keyName)
			}
			require.Equal(t, want, got)
			// The abandoned attempt holds the session lock until it
			// returns, and a retry meanwhile is told the session is busy.
			require.Eventually(t, func() bool {
				held, err := stack.rdb.Exists(context.Background(),
					"ag:demostore:cs:"+id+":lock").Result()
				return err == nil && held == 0
			}, 10*time.Second, 20*time.Millisecond)

			assert.Equal(t, "completed",
				surface.complete(context.Background(), id, key),
				"the retry is answered with the order")
			assert.Equal(t, before+1, stack.faults.orderCreates.Load(),
				"and places nothing")
		})
	}
}

//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/acp"
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
	// submit is what the call carries beyond the least a completion
	// needs: members of ACP's body, or of UCP's checkout object.
	complete func(
		ctx context.Context, id, key string, submit map[string]any,
	) string
	// answer is complete with the whole answer. It asserts nothing, so a
	// test may call it from another goroutine.
	answer func(
		ctx context.Context, id, key string, submit map[string]any,
	) surfaceAnswer
	// update changes the buyer's email, after reading the store's pay
	// ways, and reports the whole answer. It asserts nothing either.
	update func(ctx context.Context, id string) surfaceAnswer
	// updateLines replaces the checkout's lines, which writes its cart.
	updateLines func(ctx context.Context, id string) surfaceAnswer
	// abandon is cancel with the whole answer. It asserts nothing.
	abandon func(ctx context.Context, id string) surfaceAnswer
	// read reads the checkout. It asserts nothing.
	read func(ctx context.Context, id string) surfaceAnswer
	// keyName is where the surface keeps that key's state in Redis.
	keyName func(id, key string) string
	// cancel abandons the checkout.
	cancel func(t *testing.T, id string)
}

// surfaceAnswer is how a surface answered one call.
type surfaceAnswer struct {
	// status is the checkout status it rendered, "" when it refused.
	status string
	// refusal is why it refused: ACP's "<http status> <code>", UCP's
	// error text.
	refusal string
	// retryAfter is ACP's Retry-After header.
	retryAfter string
	// errorBody is ACP's error as sent, for the spec's Error schema.
	errorBody []byte
	// continueURL is the rendered checkout's continue_url.
	continueURL string
	// text is what the rendered checkout says beside its status: UCP's
	// summary text, ACP's messages.
	text string
}

// acpAnswer sends an ACP request and reads its answer: a POST under key,
// or a GET when key is empty. It asserts nothing, so it runs on any
// goroutine.
func acpAnswer(
	ctx context.Context, url, key string, body map[string]any,
) surfaceAnswer {
	raw, err := json.Marshal(body)
	if err != nil {
		return surfaceAnswer{refusal: err.Error()}
	}
	method := http.MethodPost
	if key == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, url,
		bytes.NewReader(raw))
	if err != nil {
		return surfaceAnswer{refusal: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+acpBearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("API-Version", acp.Version)
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return surfaceAnswer{refusal: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	sent, err := io.ReadAll(resp.Body)
	if err != nil {
		return surfaceAnswer{refusal: err.Error()}
	}
	var out map[string]any
	if err := json.Unmarshal(sent, &out); err != nil {
		return surfaceAnswer{refusal: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		code, _ := out["code"].(string)
		return surfaceAnswer{
			refusal:    fmt.Sprintf("%d %s", resp.StatusCode, code),
			retryAfter: resp.Header.Get("Retry-After"), errorBody: sent,
		}
	}
	return rendered(out)
}

// mcpAnswer calls a UCP tool and reads its answer. It asserts nothing,
// so it runs on any goroutine.
func mcpAnswer(
	ctx context.Context, session *mcp.ClientSession, tool string,
	args map[string]any,
) surfaceAnswer {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: tool, Arguments: args,
	})
	if err != nil {
		return surfaceAnswer{refusal: err.Error()}
	}
	if res.IsError {
		refusal := "refused"
		if len(res.Content) > 0 {
			if text, ok := res.Content[0].(*mcp.TextContent); ok {
				refusal = text.Text
			}
		}
		return surfaceAnswer{refusal: refusal}
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return surfaceAnswer{refusal: err.Error()}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return surfaceAnswer{refusal: err.Error()}
	}
	a := rendered(out)
	if len(res.Content) > 0 {
		if text, ok := res.Content[0].(*mcp.TextContent); ok {
			a.text = text.Text
		}
	}
	return a
}

// rendered is the answer of a call that rendered the checkout.
func rendered(out map[string]any) surfaceAnswer {
	a := surfaceAnswer{}
	a.status, _ = out["status"].(string)
	a.continueURL, _ = out["continue_url"].(string)
	// ACP's messages; UCP's summary text replaces this.
	if messages, err := json.Marshal(out["messages"]); err == nil {
		a.text = string(messages)
	}
	return a
}

// codPayment is UCP's payment object selecting cash on delivery, the
// fake store's pay way 1.
func codPayment() map[string]any {
	return map[string]any{"instruments": []any{map[string]any{
		"handler_id": ucp.HandlerID,
		"type":       ucp.InstrumentCashOnDelivery, "selected": true,
	}}}
}

func completionSurfaces(t *testing.T, stack ucpStack) []completionSurface {
	t.Helper()
	base := stack.gw.URL + "/acp/checkout_sessions"
	acpSurface := completionSurface{
		name: "acp",
		ready: func(t *testing.T) string {
			t.Helper()
			return acpReady(t, base)
		},
		answer: func(
			ctx context.Context, id, key string, submit map[string]any,
		) surfaceAnswer {
			body := map[string]any{"payment_data": map[string]any{}}
			for member, value := range submit {
				body[member] = value
			}
			return acpAnswer(ctx, base+"/"+id+"/complete", key, body)
		},
		update: func(ctx context.Context, id string) surfaceAnswer {
			return acpAnswer(ctx, base+"/"+id, uuid.NewString(),
				otherBuyer())
		},
		updateLines: func(ctx context.Context, id string) surfaceAnswer {
			return acpAnswer(ctx, base+"/"+id, uuid.NewString(),
				map[string]any{"line_items": []map[string]any{
					{"id": "1", "quantity": 3},
				}})
		},
		abandon: func(ctx context.Context, id string) surfaceAnswer {
			return acpAnswer(ctx, base+"/"+id+"/cancel", uuid.NewString(),
				nil)
		},
		read: func(ctx context.Context, id string) surfaceAnswer {
			return acpAnswer(ctx, base+"/"+id, "", nil)
		},
		keyName: func(id, key string) string {
			return "ag:demostore:acp:idem:complete:" + id + ":" + key
		},
		cancel: func(t *testing.T, id string) {
			t.Helper()
			resp, _ := acpCall(t, http.MethodPost, base+"/"+id+"/cancel",
				nil, uuid.NewString())
			require.Equal(t, http.StatusOK, resp.StatusCode)
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
					"payment": codPayment(),
				},
			})
			require.False(t, res.IsError, "%v", res.Content)
			created := structured(t, res)
			require.Equal(t, "ready_for_complete", created["status"])
			return created["id"].(string)
		},
		answer: func(
			ctx context.Context, id, key string, submit map[string]any,
		) surfaceAnswer {
			if submit == nil {
				submit = map[string]any{}
			}
			return mcpAnswer(ctx, session, "complete_checkout",
				map[string]any{
					"meta": meta(key), "id": id, "checkout": submit,
				})
		},
		update: func(ctx context.Context, id string) surfaceAnswer {
			return mcpAnswer(ctx, session, "update_checkout",
				map[string]any{
					"meta": meta(""), "id": id,
					"checkout": map[string]any{
						"buyer":   map[string]any{"email": otherBuyerEmail},
						"payment": codPayment(),
					},
				})
		},
		updateLines: func(ctx context.Context, id string) surfaceAnswer {
			return mcpAnswer(ctx, session, "update_checkout",
				map[string]any{
					"meta": meta(""), "id": id,
					"checkout": map[string]any{"line_items": []any{
						map[string]any{
							"item": map[string]any{"id": "1"}, "quantity": 3,
						},
					}},
				})
		},
		abandon: func(ctx context.Context, id string) surfaceAnswer {
			return mcpAnswer(ctx, session, "cancel_checkout",
				map[string]any{"meta": meta(uuid.NewString()), "id": id})
		},
		read: func(ctx context.Context, id string) surfaceAnswer {
			return mcpAnswer(ctx, session, "get_checkout",
				map[string]any{"meta": meta(""), "id": id})
		},
		keyName: func(id, key string) string {
			return "ag:demostore:idem:" + id + ":" + key
		},
		cancel: func(t *testing.T, id string) {
			t.Helper()
			res := callTool(t, session, "cancel_checkout", map[string]any{
				"meta": meta(uuid.NewString()), "id": id,
			})
			require.False(t, res.IsError, "%v", res.Content)
		},
	}
	surfaces := []completionSurface{acpSurface, mcpSurface}
	for i := range surfaces {
		answer := surfaces[i].answer
		surfaces[i].complete = func(
			ctx context.Context, id, key string, submit map[string]any,
		) string {
			return answer(ctx, id, key, submit).status
		}
	}
	return surfaces
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
	store := checkout.NewStore(stack.rdb, ucpUpstreamTimeout)
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
	case strings.HasPrefix(state, keyInFlight+":"):
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
		// untouched: the completion must not write the session at all.
		untouched bool
	}{
		{name: "an order placed", want: bookkeeping{
			Session: checkout.StatusCompleted, Linked: true, Key: keySpent,
		}},
		{name: "an order the store did not confirm",
			fault: &stack.faults.orderCreate, want: bookkeeping{
				Session: checkout.StatusCompleteInProgress, Key: keySpent,
			}},
		{name: "an order the store refused",
			fault: &stack.faults.orderRefuse, want: bookkeeping{
				Session: checkout.StatusReadyForComplete, Key: keyReleased,
			}},
		// Refused before the order is sent, by a call that submits
		// nothing new: neither the handler nor the flow has anything
		// to store.
		{name: "stock that ran out", fault: &stack.faults.stockShort,
			untouched: true, want: bookkeeping{
				Session: checkout.StatusReadyForComplete, Key: keyReleased,
			}},
	}
	for _, outcome := range outcomes {
		for _, surface := range surfaces {
			t.Run(outcome.name+" over "+surface.name, func(t *testing.T) {
				ctx := context.Background()
				id := surface.ready(t)
				if outcome.fault != nil {
					outcome.fault.Store(true)
					t.Cleanup(func() { outcome.fault.Store(false) })
				}
				before := stack.storedBytes(t, id)
				creates := stack.faults.orderCreates.Load()
				key := uuid.NewString()
				surface.complete(ctx, id, key, nil)

				assert.Equal(t, outcome.want,
					stack.bookkeeping(t, id, surface.keyName(id, key)))
				if outcome.untouched {
					assert.Equal(t, before, stack.storedBytes(t, id),
						"the stored session, its updatedAt included")
					assert.Equal(t, creates,
						stack.faults.orderCreates.Load())
				}
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
				surface.complete(ctx, id, key, nil)
			}()

			// The store holds its answer while the test waits, and it
			// must answer before the gateway gives up on it: past the
			// upstream timeout the outcome is unknown, another case.
			const wait = ucpUpstreamTimeout / 2
			select {
			case <-reached:
			case <-time.After(wait):
				t.Fatal("the order was never sent to the store")
			}
			stack.faults.orderHold.Store(nil)
			served := watch.last()
			hangUp()
			select {
			case <-served.Done():
			case <-time.After(wait):
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
					lockRedisKey(id)).Result()
				return err == nil && held == 0
			}, 10*time.Second, 20*time.Millisecond)
			// The order was not read back for a caller that had gone,
			// and the log says which order went unanswered, under the
			// request and store it was placed for.
			placed := stack.logs.with("order placed", "session="+id)
			assert.Contains(t, placed, "order="+fixtureOrderUUID)
			assert.Contains(t, placed, "request_id=")
			assert.Contains(t, placed, "tenant=demostore")

			assert.Equal(t, "completed",
				surface.complete(context.Background(), id, key, nil),
				"the retry is answered with the order")
			assert.Equal(t, before+1, stack.faults.orderCreates.Load(),
				"and places nothing")
		})
	}
}

// A platform that closes its sending side and keeps reading looks like
// one that hung up: its request's context ends the same way. It is still
// there to read, so a completed order is answered, not left with an
// empty reply: the order is placed, and the session is to be read again.
func TestACPCompleteAnswersAPlatformThatOnlyLooksGone(t *testing.T) {
	watch := new(requestWatch)
	stack := startUCPGatewayBehind(t, watch.front)
	var acpSurface completionSurface
	for _, surface := range completionSurfaces(t, stack) {
		if surface.name == "acp" {
			acpSurface = surface
		}
	}
	id := acpSurface.ready(t)
	reached, answer := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(answer) })
	defer release()
	hold := func(*http.Request) {
		close(reached)
		<-answer
	}
	stack.faults.orderHold.Store(&hold)

	host := stack.gw.Listener.Addr().String()
	conn, err := net.Dial("tcp", host)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(30*time.Second)))
	const body = `{"payment_data": {}}`
	_, err = fmt.Fprintf(conn, "POST /acp/checkout_sessions/%s/complete "+
		"HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"+
		"Content-Type: application/json\r\nAPI-Version: %s\r\n"+
		"Idempotency-Key: %s\r\nContent-Length: %d\r\n\r\n%s",
		id, host, acpBearerToken, acp.Version, uuid.NewString(), len(body),
		body)
	require.NoError(t, err)

	const wait = ucpUpstreamTimeout / 2
	select {
	case <-reached:
	case <-time.After(wait):
		t.Fatal("the order was never sent to the store")
	}
	served := watch.last()
	require.NoError(t, conn.(*net.TCPConn).CloseWrite())
	select {
	case <-served.Done():
	case <-time.After(wait):
		t.Fatal("the gateway never saw the platform's side close")
	}
	release()

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var answered map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&answered))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, answered["message"],
		"Order "+fixtureOrderUUID+" is placed")
	assert.Contains(t, answered["message"], "Read the session again")
	assert.Contains(t,
		stack.logs.with("acp: order placed", "session="+id),
		"order="+fixtureOrderUUID)
	assert.Equal(t, checkout.StatusCompleted,
		stack.storedSession(t, id).Status)
}

const otherBuyerEmail = "other@example.test"

// otherBuyer is a buyer a complete can submit in place of the one the
// surfaces' ready checkouts hold.
func otherBuyer() map[string]any {
	return map[string]any{"buyer": map[string]any{"email": otherBuyerEmail}}
}

// changeBy is a complete's submission that differs from what the
// surface's ready checkout holds: cash on delivery on UCP, where pay way
// 2 is the hosted card, and the buyer on ACP.
func changeBy(surface string) map[string]any {
	if surface == "ucp" {
		return map[string]any{"pay_way_id": 2}
	}
	return otherBuyer()
}

func (stack ucpStack) storedSession(
	t *testing.T, id string,
) *checkout.Session {
	t.Helper()
	s, err := checkout.NewStore(stack.rdb, ucpUpstreamTimeout).
		Load(context.Background(), "demostore", id)
	require.NoError(t, err)
	return s
}

// storedBytes is the session as Redis holds it: any write changes it,
// through updatedAt if through nothing else.
func (stack ucpStack) storedBytes(t *testing.T, id string) string {
	t.Helper()
	raw, err := stack.rdb.Get(context.Background(),
		sessionRedisKey(id)).Result()
	require.NoError(t, err)
	return raw
}

// What a complete call submits is the buyer's latest word, whether or not
// that call places the order. A refused one keeps it, so the next
// complete, which need not repeat it, places the order with it and not
// with what the checkout held before.
func TestRefusedCompleteKeepsWhatItSubmitted(t *testing.T) {
	stack := startUCPGateway(t)
	surfaces := map[string]completionSurface{}
	for _, surface := range completionSurfaces(t, stack) {
		surfaces[surface.name] = surface
	}
	payWay := func(s *checkout.Session) any { return s.PayWayID }
	// refusal is a complete the store refuses, and what the checkout
	// holds once it has.
	type refusal struct {
		submit map[string]any
		stored any
	}
	cases := []struct {
		name, surface string
		// refused come in order: the last is the buyer's latest word,
		// and what the order must carry.
		refused []refusal
		// member is where an order's body carries what was submitted.
		member string
		read   func(*checkout.Session) any
	}{
		{
			name: "a hosted pay way", surface: "ucp",
			refused: []refusal{{map[string]any{"pay_way_id": 2}, 2}},
			member:  "payWayId", read: payWay,
		},
		{
			// The first refusal leaves the hosted card selected, so
			// cash on delivery after the second is the instrument's
			// doing.
			name: "an instrument", surface: "ucp",
			refused: []refusal{
				{map[string]any{"pay_way_id": 2}, 2},
				{map[string]any{"payment": codPayment()}, 1},
			},
			member: "payWayId", read: payWay,
		},
		{
			name: "a buyer", surface: "acp",
			refused: []refusal{{otherBuyer(), otherBuyerEmail}},
			member:  "email",
			read:    func(s *checkout.Session) any { return s.Buyer.Email },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+" over "+tc.surface, func(t *testing.T) {
			ctx := context.Background()
			surface := surfaces[tc.surface]
			id := surface.ready(t)
			creates := stack.faults.orderCreates.Load()

			stack.faults.stockShort.Store(true)
			t.Cleanup(func() { stack.faults.stockShort.Store(false) })
			for _, refused := range tc.refused {
				key := uuid.NewString()
				require.Empty(t,
					surface.complete(ctx, id, key, refused.submit),
					"refused: the stock ran out")
				assert.Equal(t, bookkeeping{
					Session: checkout.StatusReadyForComplete,
					Key:     keyReleased,
				}, stack.bookkeeping(t, id, surface.keyName(id, key)))
				require.EqualValues(t, refused.stored,
					tc.read(stack.storedSession(t, id)))
			}
			stack.faults.stockShort.Store(false)
			require.Equal(t, creates, stack.faults.orderCreates.Load())

			require.NotEmpty(t,
				surface.complete(ctx, id, uuid.NewString(), nil),
				"the next complete places the order")
			require.Equal(t, creates+1, stack.faults.orderCreates.Load())
			assert.EqualValues(t,
				tc.refused[len(tc.refused)-1].stored,
				(*stack.faults.lastOrder.Load())[tc.member])
		})
	}
}

// On ACP the buyer arrives with complete, ahead of every check the call
// can fail: it is kept when the session is not ready yet, and when the
// store's pay ways cannot be read.
func TestACPCompleteKeepsTheBuyerItCouldNotOrderFor(t *testing.T) {
	stack := startUCPGateway(t)
	ctx := context.Background()
	var acpSurface completionSurface
	for _, surface := range completionSurfaces(t, stack) {
		if surface.name == "acp" {
			acpSurface = surface
		}
	}

	t.Run("a session that is not ready", func(t *testing.T) {
		resp, created := acpCall(t, http.MethodPost,
			stack.gw.URL+"/acp/checkout_sessions", map[string]any{
				"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
				"currency":     "EUR",
				"capabilities": map[string]any{},
			}, uuid.NewString())
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		id := created["id"].(string)

		key := uuid.NewString()
		require.Empty(t, acpSurface.complete(ctx, id, key, otherBuyer()))

		assert.Equal(t, bookkeeping{
			Session: checkout.StatusIncomplete, Key: keyReleased,
		}, stack.bookkeeping(t, id, acpSurface.keyName(id, key)))
		assert.Equal(t, otherBuyerEmail,
			stack.storedSession(t, id).Buyer.Email)
	})

	t.Run("pay ways that cannot be read", func(t *testing.T) {
		id := acpSurface.ready(t)
		creates := stack.faults.orderCreates.Load()

		stack.faults.payWayLookup.Store(true)
		t.Cleanup(func() { stack.faults.payWayLookup.Store(false) })
		key := uuid.NewString()
		require.Empty(t, acpSurface.complete(ctx, id, key, otherBuyer()))
		stack.faults.payWayLookup.Store(false)

		assert.Equal(t, bookkeeping{
			Session: checkout.StatusReadyForComplete, Key: keyReleased,
		}, stack.bookkeeping(t, id, acpSurface.keyName(id, key)))
		assert.Equal(t, otherBuyerEmail,
			stack.storedSession(t, id).Buyer.Email)

		require.NotEmpty(t, acpSurface.complete(ctx, id, uuid.NewString(), nil))
		require.Equal(t, creates+1, stack.faults.orderCreates.Load())
		assert.Equal(t, otherBuyerEmail,
			(*stack.faults.lastOrder.Load())["email"])
	})
}

// A UCP complete whose key another attempt holds, or has finished with,
// is that attempt's duplicate. The lock can lapse while a complete looks
// up its payment, so the other attempt may have placed the order by now:
// the duplicate writes nothing, or it would put its own snapshot over a
// placed session.
func TestCompleteWithoutItsClaimWritesNothing(t *testing.T) {
	stack := startUCPGateway(t)
	var ucpSurface completionSurface
	for _, surface := range completionSurfaces(t, stack) {
		if surface.name == "ucp" {
			ucpSurface = surface
		}
	}
	for name, claim := range map[string]string{
		"another attempt's": "inflight:" + uuid.NewString(), "done": "done",
	} {
		t.Run("a key that is "+name, func(t *testing.T) {
			ctx := context.Background()
			id := ucpSurface.ready(t)
			key := uuid.NewString()
			require.NoError(t, stack.rdb.Set(ctx,
				ucpSurface.keyName(id, key), claim, time.Minute).Err())
			before := stack.storedBytes(t, id)
			creates := stack.faults.orderCreates.Load()

			ucpSurface.complete(ctx, id, key, changeBy("ucp"))

			assert.Equal(t, before, stack.storedBytes(t, id))
			assert.Equal(t, creates, stack.faults.orderCreates.Load())
		})
	}
}

// A checkout that is canceled, or that has placed its order, is closed:
// a complete that submits something new is refused and changes nothing
// in it.
func TestCompleteLeavesAClosedSessionAsItIs(t *testing.T) {
	stack := startUCPGateway(t)
	ctx := context.Background()
	surfaces := map[string]completionSurface{}
	for _, surface := range completionSurfaces(t, stack) {
		surfaces[surface.name] = surface
	}

	for name, surface := range surfaces {
		t.Run("canceled, over "+name, func(t *testing.T) {
			id := surface.ready(t)
			surface.cancel(t, id)
			before := stack.storedBytes(t, id)

			require.Empty(t, surface.complete(
				ctx, id, uuid.NewString(), changeBy(name)))

			assert.Equal(t, before, stack.storedBytes(t, id))
		})
	}

	// A checkout answers on either surface by its id. UCP retries the
	// payment link of an escalated one, so ACP is where a complete could
	// write to it.
	t.Run("awaiting payment, over acp", func(t *testing.T) {
		id := surfaces["ucp"].ready(t)
		require.Equal(t, "requires_escalation", surfaces["ucp"].complete(
			ctx, id, uuid.NewString(), changeBy("ucp")))
		before := stack.storedBytes(t, id)

		// The order a complete asks for exists: the session is answered
		// as it is, and what the call submitted is not stored.
		answer := surfaces["acp"].answer(
			ctx, id, uuid.NewString(), changeBy("acp"))
		require.Equal(t, "requires_escalation", answer.status)
		// continue_url is the payment page the store opened for the
		// order, as its message says, and the message names the order.
		assert.Equal(t, vivaCheckoutURL, answer.continueURL)
		assert.Contains(t, answer.text, "Order "+fixtureOrderUUID+
			" is placed and awaits its payment")

		assert.Equal(t, before, stack.storedBytes(t, id))
	})
}

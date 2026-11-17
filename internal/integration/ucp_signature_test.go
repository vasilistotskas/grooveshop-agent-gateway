//go:build integration

package integration

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpsig"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// signingPlatform is a UCP platform that signs its requests to /mcp
// (RFC 9421) with a key its profile publishes.
type signingPlatform struct {
	key     *ucp.SigningKey
	profile string
	sink    *webhookSink
}

func newSigningPlatform(t *testing.T) signingPlatform {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	scalar, err := priv.Bytes()
	require.NoError(t, err)
	key, err := ucp.SigningKeyFromScalar(scalar)
	require.NoError(t, err)
	sink := newWebhookSink()
	hooks := httptest.NewServer(sink.handler())
	t.Cleanup(hooks.Close)
	jwk, err := json.Marshal(key.JWK())
	require.NoError(t, err)
	profile := servePlatformProfile(t, hooks.URL+"/ucp/orders",
		func(body string) string {
			i := strings.LastIndex(body, "}")
			return body[:i] + `,"keys":[` + string(jwk) + "]}"
		})
	return signingPlatform{key: key, profile: profile, sink: sink}
}

// RoundTrip signs each request as UCP's MCP transport binding asks
// (ucp.SignRequest).
func (p signingPlatform) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	var body []byte
	if req.Body != nil {
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	if err := ucp.SignRequest(req, body, p.profile, p.key,
		time.Now()); err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(req)
}

// connect opens an MCP session whose every request p signs.
func (p signingPlatform) connect(t *testing.T, gatewayURL string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{
		Name: "e2e-signing-platform", Version: "test",
	}, nil)
	session, err := client.Connect(t.Context(),
		&mcp.StreamableClientTransport{
			Endpoint:             gatewayURL + "/mcp",
			DisableStandaloneSSE: true,
			HTTPClient:           &http.Client{Transport: p},
		}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func (p signingPlatform) meta(idem string) map[string]any {
	m := map[string]any{"ucp-agent": map[string]any{"profile": p.profile}}
	if idem != "" {
		m["idempotency-key"] = idem
	}
	return m
}

// placeOrder creates a checkout of the fixture cart and completes it,
// over session as the platform meta names, and returns its id.
func placeOrder(
	t *testing.T, session *mcp.ClientSession,
	meta func(string) map[string]any,
) string {
	t.Helper()
	res := callTool(t, session, "create_checkout", map[string]any{
		"meta": meta(""), "cart_id": fixtureCartID,
		"checkout": map[string]any{
			"buyer": map[string]any{
				"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
				"email": "maria@example.test", "phone_number": "+306912345678",
			},
			"fulfillment": map[string]any{
				"kind": "home_delivery", "providerCode": "acs",
				"countryCode": "GR", "city": "Αθήνα", "zipcode": "10431",
				"street": "Πανεπιστημίου", "streetNumber": "12",
				"regionId": "GR-14",
			},
			"payment": codPayment(),
		},
	})
	require.False(t, res.IsError, "%v", res.Content)
	id := structured(t, res)["id"].(string)
	res = callTool(t, session, "complete_checkout", map[string]any{
		"meta": meta(uuid.NewString()), "id": id,
		"checkout": map[string]any{"payment": codPayment()},
	})
	require.False(t, res.IsError, "%v", res.Content)
	assert.Equal(t, fixtureOrderUUID,
		structured(t, res)["order"].(map[string]any)["id"])
	return id
}

// orderReads counts the order reads the store's Django receives.
func orderReads(stack ucpStack) *atomic.Int32 {
	var n atomic.Int32
	count := func(r *http.Request) {
		if strings.Contains(r.URL.Path, "/order/") &&
			r.Method == http.MethodGet {
			n.Add(1)
		}
	}
	stack.faults.before.Store(&count)
	return &n
}

// get_order serves an order to the platform whose signed create_checkout
// placed it, and to no other: any other platform's get_order, for that
// order or for an id that names none, gets the same not_found, and the
// store is not asked.
func TestGetOrderServesOnlyThePlatformThatPlacedIt(t *testing.T) {
	stack := startUCPGateway(t)
	owner := newSigningPlatform(t)
	other := newSigningPlatform(t)
	ownerSession := owner.connect(t, stack.gw.URL)
	otherSession := other.connect(t, stack.gw.URL)

	checkoutID := placeOrder(t, ownerSession, owner.meta)

	res := callTool(t, ownerSession, "get_order", map[string]any{
		"meta": owner.meta(""), "id": fixtureOrderUUID,
	})
	require.False(t, res.IsError, "%v", res.Content)
	order := structured(t, res)
	assert.Equal(t, fixtureOrderUUID, order["id"])
	assert.Equal(t, checkoutID, order["checkout_id"])

	reads := orderReads(stack)
	placed := callTool(t, otherSession, "get_order", map[string]any{
		"meta": other.meta(""), "id": fixtureOrderUUID,
	})
	unknown := callTool(t, otherSession, "get_order", map[string]any{
		"meta": other.meta(""), "id": uuid.NewString(),
	})
	assert.Zero(t, reads.Load(), "the store is not asked")
	require.False(t, placed.IsError)
	notFound := structured(t, placed)
	assert.Equal(t, notFound, structured(t, unknown),
		"another platform's order reads as an id that names none")
	assert.Equal(t, "error", notFound["ucp"].(map[string]any)["status"])
	msg := notFound["messages"].([]any)[0].(map[string]any)
	assert.Equal(t, "not_found", msg["code"])
	assert.Equal(t, "unrecoverable", msg["severity"])

	// The checkout the order came from is the owner's alone, too.
	assert.Contains(t, checkoutGone(t, callTool(t, otherSession,
		"get_checkout", map[string]any{
			"meta": other.meta(""), "id": checkoutID,
		})), "an order may exist")
	got := callTool(t, ownerSession, "get_checkout", map[string]any{
		"meta": owner.meta(""), "id": checkoutID,
	})
	require.False(t, got.IsError, "%v", got.Content)

	// The owner's order events still reach it.
	require.Equal(t, http.StatusNoContent,
		stack.pushOrderEvent(t, "COMPLETED"))
	select {
	case <-owner.sink.received:
	case <-time.After(15 * time.Second):
		t.Fatal("the owner's order webhook was never delivered")
	}
}

// An unsigned get_order is refused before the tool runs, with the spec's
// 401 and signature_missing.
func TestGetOrderRefusesAnUnsignedRequest(t *testing.T) {
	stack := startUCPGateway(t)
	reads := orderReads(stack)
	body := `{"jsonrpc":"2.0","id":"q1","method":"tools/call","params":` +
		`{"name":"get_order","arguments":{"meta":{"ucp-agent":{"profile":` +
		`"https://platform.example/ucp"}},"id":"` + fixtureOrderUUID + `"}}}`
	req, err := http.NewRequest(http.MethodPost, stack.gw.URL+"/mcp",
		strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	var out struct {
		ID    string `json:"id"`
		Error struct {
			Code int64 `json:"code"`
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "q1", out.ID)
	assert.EqualValues(t, -32000, out.Error.Code)
	assert.Equal(t, httpsig.CodeSignatureMissing, out.Error.Data.Code)
	assert.Zero(t, reads.Load())
}

// A checkout a platform signed the creation of answers that platform
// alone: another platform's calls, and unsigned ones, find no checkout.
// One created unsigned keeps answering any caller, and its order is no
// platform's to read with get_order.
func TestACheckoutServesThePlatformThatSignedItsCreation(t *testing.T) {
	stack := startUCPGateway(t)
	owner := newSigningPlatform(t)
	other := newSigningPlatform(t)
	ownerSession := owner.connect(t, stack.gw.URL)
	otherSession := other.connect(t, stack.gw.URL)
	anonymous := connectMCP(t, stack.gw.URL)

	res := callTool(t, ownerSession, "create_checkout", map[string]any{
		"meta": owner.meta(""),
		"checkout": map[string]any{"line_items": []any{map[string]any{
			"item": map[string]any{"id": "1"}, "quantity": 1,
		}}},
	})
	require.False(t, res.IsError, "%v", res.Content)
	id := structured(t, res)["id"].(string)

	lines := []any{map[string]any{
		"item": map[string]any{"id": "1"}, "quantity": 2,
	}}
	for _, caller := range []struct {
		name    string
		session *mcp.ClientSession
		meta    func(string) map[string]any
	}{
		{"another platform", otherSession, other.meta},
		{"an unsigned caller", anonymous, owner.meta},
	} {
		for tool, args := range map[string]map[string]any{
			"get_checkout": {"meta": caller.meta(""), "id": id},
			"update_checkout": {"meta": caller.meta(""), "id": id,
				"checkout": map[string]any{"line_items": lines}},
			"complete_checkout": {"meta": caller.meta(uuid.NewString()),
				"id": id, "checkout": map[string]any{"payment": codPayment()}},
			"cancel_checkout": {"meta": caller.meta(uuid.NewString()),
				"id": id},
		} {
			got := callTool(t, caller.session, tool, args)
			assert.Contains(t, checkoutGone(t, got),
				"an order may exist", "%s: %s", caller.name, tool)
		}
	}

	res = callTool(t, ownerSession, "cancel_checkout", map[string]any{
		"meta": owner.meta(uuid.NewString()), "id": id,
	})
	require.False(t, res.IsError, "%v", res.Content)
	assert.Equal(t, "canceled", structured(t, res)["status"])

	// Unsigned, a checkout is anyone's, and its order no platform's.
	unsigned := newSigningPlatform(t)
	checkoutID := placeOrder(t, anonymous, unsigned.meta)
	got := callTool(t, anonymous, "get_checkout", map[string]any{
		"meta": unsigned.meta(""), "id": checkoutID,
	})
	require.False(t, got.IsError, "%v", got.Content)
	order := callTool(t, unsigned.connect(t, stack.gw.URL), "get_order",
		map[string]any{"meta": unsigned.meta(""), "id": fixtureOrderUUID})
	require.False(t, order.IsError)
	msg := structured(t, order)["messages"].([]any)[0].(map[string]any)
	assert.Equal(t, "not_found", msg["code"])
}

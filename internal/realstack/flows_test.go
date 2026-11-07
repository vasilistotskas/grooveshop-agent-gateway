//go:build realstack

package realstack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requestTimeout bounds one call, so a hung service fails its test rather than
// waiting for the whole run's timeout.
const requestTimeout = time.Minute

var client = &http.Client{Timeout: requestTimeout}

// hostTransport sends every request under the store's Host, as its domain
// would.
type hostTransport struct {
	host  string
	token string
}

func (h hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = h.host
	if h.token != "" {
		r.Header.Set("Authorization", "Bearer "+h.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(
		&mcp.Implementation{Name: "real-stack", Version: "test"}, nil)
	session, err := client.Connect(context.Background(),
		&mcp.StreamableClientTransport{
			Endpoint: running.gatewayURL + "/mcp",
			HTTPClient: &http.Client{
				Timeout:   requestTimeout,
				Transport: hostTransport{storeHost, token},
			},
			DisableStandaloneSSE: true,
		}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(
	t *testing.T, s *mcp.ClientSession, name string, args map[string]any,
) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name: name, Arguments: args,
	})
	require.NoError(t, err)
	return res
}

func errorText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.True(t, res.IsError, "expected a tool error")
	return res.Content[0].(*mcp.TextContent).Text
}

func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		require.Failf(t, "tool error", "%s",
			res.Content[0].(*mcp.TextContent).Text)
	}
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func productID(t *testing.T, s *mcp.ClientSession) string {
	t.Helper()
	found := structured(t, call(t, s, "search_products", map[string]any{
		"query": "θήκη", "limit": 1,
	}))
	products := found["products"].([]any)
	require.NotEmpty(t, products, "the seeded store must be searchable")
	id, err := json.Marshal(products[0].(map[string]any)["id"])
	require.NoError(t, err)
	return string(id)
}

func meta(idempotencyKey string) map[string]any {
	m := map[string]any{
		"ucp-agent": map[string]any{"profile": running.profileURL},
	}
	if idempotencyKey != "" {
		m["idempotency-key"] = idempotencyKey
	}
	return m
}

var cashOnDelivery = map[string]any{"instruments": []any{map[string]any{
	"handler_id": "grooveshop_payments",
	"type":       "cash_on_delivery", "selected": true,
}}}

// placeUCPOrder buys one unit through the canonical tools.
func placeUCPOrder(
	t *testing.T, s *mcp.ClientSession, product, country string,
) (checkoutID, orderID string) {
	t.Helper()
	created := structured(t, call(t, s, "create_checkout", map[string]any{
		"meta": meta(""),
		"checkout": map[string]any{"line_items": []any{map[string]any{
			"item": map[string]any{"id": product}, "quantity": 1,
		}}},
	}))
	assert.Equal(t, "incomplete", created["status"])
	checkoutID = created["id"].(string)

	updated := structured(t, call(t, s, "update_checkout", map[string]any{
		"meta": meta(""), "id": checkoutID,
		"checkout": map[string]any{
			"buyer": map[string]any{
				"first_name": "Real", "last_name": "Stack",
				"email":        "real-stack@example.test",
				"phone_number": "+306912345678",
			},
			"fulfillment": map[string]any{
				"kind": "home_delivery", "providerCode": "flat_rate",
				"countryCode": country, "city": "Αθήνα", "zipcode": "10431",
				"street": "Πανεπιστημίου", "streetNumber": "12",
				"regionId": "GR-14",
			},
			"payment": cashOnDelivery,
		},
	}))
	require.Equal(t, "ready_for_complete", updated["status"], "%v", updated["messages"])

	done := structured(t, call(t, s, "complete_checkout", map[string]any{
		"meta": meta(uuid.NewString()), "id": checkoutID,
		"checkout": map[string]any{"payment": cashOnDelivery},
	}))
	require.Equal(t, "completed", done["status"])
	return checkoutID, done["order"].(map[string]any)["id"].(string)
}

func TestUCPOrderReadsBackWithTheSameTotals(t *testing.T) {
	s := connect(t, "")
	checkoutID, orderID := placeUCPOrder(t, s, productID(t, s), "GR")

	placed := structured(t, call(t, s, "get_checkout", map[string]any{
		"meta": meta(""), "id": checkoutID,
	}))
	order := structured(t, call(t, s, "get_order", map[string]any{
		"meta": meta(""), "id": orderID,
	}))
	assert.Equal(t, checkoutID, order["checkout_id"])
	assert.Equal(t, totalOf(t, placed), totalOf(t, order),
		"a checkout and its order must agree")
}

// ageOrder moves an order past what a guest's UUID opens.
func ageOrder(t *testing.T, orderID string) {
	t.Helper()
	script := fmt.Sprintf(`
from datetime import timedelta
from django.conf import settings
from django.utils import timezone
from django_tenants.utils import schema_context
from order.models.order import Order
with schema_context("demostore"):
    aged = timezone.now() - timedelta(days=settings.GUEST_ORDER_ACCESS_DAYS + 1)
    assert Order.objects.filter(uuid=%q).update(created_at=aged) == 1
`, orderID)
	code, out, err := running.django.Exec(context.Background(), []string{
		".venv/bin/python", "manage.py", "shell", "-c", script,
	})
	require.NoError(t, err)
	output, err := io.ReadAll(out)
	require.NoError(t, err)
	require.Zero(t, code, tail(string(output)))
}

// Past that limit a guest's UUID opens nothing, so an agent holding one is told
// the order does not exist, while the gateway still reads the order it placed.
func TestAgedOrderStaysReadableOnlyToTheGateway(t *testing.T) {
	s := connect(t, "")
	checkoutID, orderID := placeUCPOrder(t, s, productID(t, s), "GR")
	ageOrder(t, orderID)

	placed := structured(t, call(t, s, "get_checkout", map[string]any{
		"meta": meta(""), "id": checkoutID,
	}))
	assert.Equal(t, "completed", placed["status"])

	// A client learns nothing from it: an aged order and no order read the same.
	const unknown = "00000000-0000-4000-8000-000000000000"
	getOrder := func(id string) string {
		return errorText(t, call(t, s, "get_order", map[string]any{
			"meta": meta(""), "id": id,
		}))
	}
	trackOrder := func(id string) string {
		return errorText(t, call(t, s, "track_order", map[string]any{
			"orderUuid": id,
		}))
	}
	assert.Contains(t, getOrder(orderID), "no order can be opened")
	assert.Equal(t, getOrder(unknown), getOrder(orderID))
	assert.Contains(t, trackOrder(orderID), "no order can be opened")
	assert.Equal(t, trackOrder(unknown), trackOrder(orderID))
}

func totalOf(t *testing.T, payload map[string]any) float64 {
	t.Helper()
	for _, row := range payload["totals"].([]any) {
		total := row.(map[string]any)
		if total["type"] == "total" {
			return total["amount"].(float64)
		}
	}
	require.Fail(t, "no total row")
	return 0
}

// Django knows a country only by its upper-case code.
func TestUCPOrderTakesALowerCaseCountry(t *testing.T) {
	s := connect(t, "")
	placeUCPOrder(t, s, productID(t, s), "gr")
}

// A placed order that cannot be read back must not read as a failure, or the
// agent would order again. Django is stopped for real, and restarted after.
func TestUCPReadFailureNamesThePlacedOrder(t *testing.T) {
	ctx := context.Background()
	s := connect(t, "")
	checkoutID, orderID := placeUCPOrder(t, s, productID(t, s), "GR")

	timeout := time.Minute
	require.NoError(t, running.django.Stop(ctx, &timeout))
	t.Cleanup(func() { require.NoError(t, running.django.Start(ctx)) })

	text := errorText(t, call(t, s, "get_checkout", map[string]any{
		"meta": meta(""), "id": checkoutID,
	}))
	assert.Contains(t, text, "order "+orderID+" is placed")
	assert.Contains(t, text, "do not start a new checkout")
}

func acpRequest(
	t *testing.T, path string, body any,
) (int, map[string]any) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewReader(raw)
	}
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, running.gatewayURL+"/acp"+path, payload)
	require.NoError(t, err)
	req.Host = storeHost
	req.Header.Set("Authorization", "Bearer "+running.seeded.ACPToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("API-Version", "2026-04-17")
	if body != nil {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return resp.StatusCode, out
}

func TestACPOrderCompletesFromACapitalisedRegionName(t *testing.T) {
	s := connect(t, "")
	status, created := acpRequest(t, "/checkout_sessions", map[string]any{
		"line_items":   []any{map[string]any{"id": productID(t, s), "quantity": 1}},
		"currency":     "EUR",
		"capabilities": map[string]any{},
		"buyer": map[string]any{
			"first_name": "Real", "last_name": "Stack",
			"email":        "real-stack@example.test",
			"phone_number": "+306912345678",
		},
		"fulfillment_details": map[string]any{
			"name": "Real Stack",
			"address": map[string]any{
				"name": "Real Stack", "line_one": "Πανεπιστημίου 12",
				"city": "Αθήνα", "state": "ΑΤΤΙΚΗ", "country": "gr",
				"postal_code": "10431",
			},
		},
	})
	require.Equal(t, http.StatusCreated, status, "%v", created)
	session := "/checkout_sessions/" + created["id"].(string)
	line := created["line_items"].([]any)[0].(map[string]any)["id"]

	status, selected := acpRequest(t, session, map[string]any{
		"selected_fulfillment_options": []any{map[string]any{
			"type": "shipping", "option_id": "flat_rate:home_delivery",
			"item_ids": []any{line},
		}},
	})
	require.Equal(t, http.StatusOK, status, "%v", selected)
	require.Equal(t, "ready_for_payment", selected["status"], "%v", selected["messages"])

	status, done := acpRequest(t, session+"/complete",
		map[string]any{"payment_data": map[string]any{}})
	require.Equal(t, http.StatusOK, status, "%v", done)
	assert.Equal(t, "completed", done["status"])
	assert.Contains(t, done["continue_url"], "/checkout/success/")

	status, read := acpRequest(t, session, nil)
	require.Equal(t, http.StatusOK, status, "%v", read)
	assert.Equal(t, "completed", read["status"])
}

func anonymousAccountCall(t *testing.T, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, running.gatewayURL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
			`"params":{"name":"my_orders","arguments":{}}}`))
	require.NoError(t, err)
	req.Host = storeHost
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestAccountToolsChallengeOrAnswer(t *testing.T) {
	t.Run("no token starts OAuth", func(t *testing.T) {
		resp := anonymousAccountCall(t, "")
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "resource_metadata=")
	})
	t.Run("a token bound to this store answers", func(t *testing.T) {
		s := connect(t, running.seeded.LinkedToken)
		// The store's plan decides which it serves, so ask it.
		tools, err := s.ListTools(context.Background(), nil)
		require.NoError(t, err)
		answered := 0
		for _, tool := range tools.Tools {
			if strings.HasPrefix(tool.Name, "my_") {
				structured(t, call(t, s, tool.Name, map[string]any{}))
				answered++
			}
		}
		assert.Positive(t, answered, "the store must serve an account tool")
	})
	// Django answers 401 for a token bound elsewhere. A 403 reads as a missing
	// scope, and the client would never be asked to link again.
	t.Run("a token bound elsewhere is challenged", func(t *testing.T) {
		resp := anonymousAccountCall(t, running.seeded.ForeignToken)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "resource_metadata=")
	})
}

// A client can only link an account on the resource the OAuth metadata names,
// so discovery must lead there from any of the store's hosts.
func TestDiscoveryNamesThePrimaryDomainOnAnyHost(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet,
		running.gatewayURL+"/mcp/server-card", nil)
	require.NoError(t, err)
	req.Host = aliasHost
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var card struct {
		Remotes []struct {
			URL string `json:"url"`
		} `json:"remotes"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&card))
	require.Len(t, card.Remotes, 1)
	assert.Equal(t, "https://"+storeHost+"/mcp", card.Remotes[0].URL)
}

//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/acp"
)

// acpErrorSchema is the vendored spec's Error, which every 4xx and 5xx
// body of the ACP surface must match.
var acpErrorSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	f, err := os.Open(filepath.Join("..", "..", "testdata", "schemas",
		"acp", acp.Version, "schema.agentic_checkout.json"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("acp-checkout.json", raw); err != nil {
		return nil, err
	}
	return c.Compile("acp-checkout.json#/$defs/Error")
})

// acpRequest builds a request the way an agentic platform sends it.
func acpRequest(
	t *testing.T, method, url string, body any, idemKey string,
) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+acpBearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("API-Version", acp.Version)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return req
}

// acpDo sends req and decodes the answer, which must be a spec Error
// whenever it is not a success.
func acpDo(
	t *testing.T, req *http.Request,
) (*http.Response, map[string]any) {
	t.Helper()
	resp, raw := acpRaw(t, req)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded), "%s", raw)
	return resp, decoded
}

// acpRaw sends req and returns the answer's bytes, which must be a spec
// Error whenever it is not a success.
func acpRaw(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if resp.StatusCode >= http.StatusBadRequest {
		schema, err := acpErrorSchema()
		require.NoError(t, err)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		require.NoError(t, err, "%s", raw)
		require.NoError(t, schema.Validate(doc), "%s", raw)
	}
	return resp, raw
}

// acpCall hits the ACP REST surface the way an agentic platform does.
func acpCall(
	t *testing.T, method, url string, body any, idemKey string,
) (*http.Response, map[string]any) {
	t.Helper()
	return acpDo(t, acpRequest(t, method, url, body, idemKey))
}

// acpReady returns the id of an ACP session at base that is ready to
// complete with cash on delivery.
func acpReady(t *testing.T, base string) string {
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
}

// Django deletes the cart with the order, so a completed session renders
// from the order, and its continue_url leads to the order, not to a cart
// that no longer exists.
func TestACPCompletedSessionRendersFromTheOrder(t *testing.T) {
	stack := startUCPGateway(t)
	stack.faults.cartDeletedOnOrder.Store(true)
	base := stack.gw.URL + "/acp/checkout_sessions"

	resp, created := acpCall(t, http.MethodPost, base, map[string]any{
		"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
		"currency":     "EUR",
		"capabilities": map[string]any{},
		"buyer": map[string]any{
			"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
			"email": "maria@example.test", "phone_number": "+306912345678",
		},
		"fulfillment_details": map[string]any{
			"name": "Μαρία Παπαδοπούλου",
			"address": map[string]any{
				"name": "Μαρία Παπαδοπούλου", "line_one": "Πανεπιστημίου 12",
				"city": "Αθήνα", "state": "GR-14", "country": "GR",
				"postal_code": "10431",
			},
		},
	}, uuid.NewString())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	session := base + "/" + created["id"].(string)
	resp, _ = acpCall(t, http.MethodPost, session, map[string]any{
		"selected_fulfillment_options": []map[string]any{{
			"type": "shipping", "option_id": "acs:home_delivery",
			"item_ids": []string{"410"},
		}},
	}, uuid.NewString())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assertRendered := func(resp *http.Response, out map[string]any) {
		t.Helper()
		require.Equal(t, http.StatusOK, resp.StatusCode, "%v", out)
		assert.Equal(t, "completed", out["status"])
		assert.Contains(t, out["continue_url"],
			"/checkout/success/"+fixtureOrderUUID)
		amounts := map[string]float64{}
		for _, row := range out["totals"].([]any) {
			total := row.(map[string]any)
			amounts[total["type"].(string)] = total["amount"].(float64)
		}
		assert.EqualValues(t, fixtureOrderTotal, amounts["total"])
	}

	// The order goes through but cannot be read back: the answer must say it
	// exists, or the platform would order again.
	stack.faults.orderRead.Store(true)
	resp, failed := acpCall(t, http.MethodPost, session+"/complete",
		map[string]any{"payment_data": map[string]any{}}, uuid.NewString())
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, failed["message"], "Order "+fixtureOrderUUID+" is placed")
	assert.Contains(t, failed["message"], "do not start a new checkout")
	stack.faults.orderRead.Store(false)

	resp, completed := acpCall(t, http.MethodPost, session+"/complete",
		map[string]any{"payment_data": map[string]any{}}, uuid.NewString())
	assertRendered(resp, completed)
	resp, fetched := acpCall(t, http.MethodGet, session, nil, "")
	assertRendered(resp, fetched)
	assert.EqualValues(t, 1, stack.faults.orderCreates.Load())

	stack.faults.orderRead.Store(true)
	resp, failed = acpCall(t, http.MethodGet, session, nil, "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, failed["message"], "Order "+fixtureOrderUUID+" is placed")
}

func TestACPEndToEnd(t *testing.T) {
	stack := startUCPGateway(t)
	gw := stack.gw
	base := gw.URL + "/acp/checkout_sessions"

	buyer := map[string]any{
		"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
		"email": "maria@example.test", "phone_number": "+306912345678",
	}
	fulfillment := map[string]any{
		"name": "Μαρία Παπαδοπούλου",
		"address": map[string]any{
			"name": "Μαρία Παπαδοπούλου", "line_one": "Πανεπιστημίου 12",
			"city": "Αθήνα", "state": "ΑΤΤΙΚΗ", "country": "GR",
			"postal_code": "10431",
		},
	}
	createBody := map[string]any{
		"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
		"currency":     "EUR",
		"capabilities": map[string]any{},
	}

	t.Run("rejects a missing or wrong bearer token", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, base,
			bytes.NewReader([]byte(`{}`)))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer wrong")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("create requires an idempotency key", func(t *testing.T) {
		resp, body := acpCall(t, http.MethodPost, base, createBody, "")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "idempotency_key_required", body["code"])
	})

	t.Run("full flow: create, update, complete with COD", func(t *testing.T) {
		createKey := uuid.NewString()
		resp, created := acpCall(t, http.MethodPost, base, createBody,
			createKey)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		assert.Equal(t, "not_ready_for_payment", created["status"])
		assert.Contains(t, created["continue_url"], "/cart/claim?uuid=")
		sessionID := created["id"].(string)

		// Same key + same body replays the same session.
		resp, replayed := acpCall(t, http.MethodPost, base, createBody,
			createKey)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		assert.Equal(t, "true", resp.Header.Get("Idempotent-Replayed"))
		assert.Equal(t, sessionID, replayed["id"])

		// Same key + different body is an idempotency conflict.
		other := map[string]any{
			"line_items":   []map[string]any{{"id": "1", "quantity": 5}},
			"currency":     "EUR",
			"capabilities": map[string]any{},
		}
		resp, conflict := acpCall(t, http.MethodPost, base, other, createKey)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
		assert.Equal(t, "idempotency_conflict", conflict["code"])

		updateBody := map[string]any{
			"buyer":               buyer,
			"fulfillment_details": fulfillment,
			"selected_fulfillment_options": []map[string]any{{
				"type": "shipping", "option_id": "acs:home_delivery",
				"item_ids": []string{"410"},
			}},
		}
		// Every POST carries a key, update included.
		resp, missing := acpCall(t, http.MethodPost, base+"/"+sessionID,
			updateBody, "")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "idempotency_key_required", missing["code"])

		resp, updated := acpCall(t, http.MethodPost, base+"/"+sessionID,
			updateBody, uuid.NewString())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "ready_for_payment", updated["status"])
		opts := updated["fulfillment_options"].([]any)
		require.NotEmpty(t, opts, "home-delivery options are offered")

		completeKey := uuid.NewString()
		resp, completed := acpCall(t, http.MethodPost,
			base+"/"+sessionID+"/complete",
			map[string]any{"payment_data": map[string]any{}}, completeKey)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "completed", completed["status"])
		order := completed["order"].(map[string]any)
		assert.Equal(t, fixtureOrderUUID, order["id"])
		assert.Equal(t, sessionID, order["checkout_session_id"])
		// The order is attributed to the surface owning the session.
		assert.Equal(t, "acp", stack.faults.agentProtocol.Load())

		// Completing again just re-renders the final state.
		resp, again := acpCall(t, http.MethodPost,
			base+"/"+sessionID+"/complete",
			map[string]any{"payment_data": map[string]any{}},
			uuid.NewString())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "completed", again["status"])

		resp, fetched := acpCall(t, http.MethodGet, base+"/"+sessionID,
			nil, "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "completed", fetched["status"])
	})

	t.Run("a currency the store does not sell in is refused",
		func(t *testing.T) {
			other := map[string]any{
				"line_items":   []map[string]any{{"id": "1", "quantity": 1}},
				"currency":     "USD",
				"capabilities": map[string]any{},
			}
			key := uuid.NewString()
			resp, refused := acpCall(t, http.MethodPost, base, other, key)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Equal(t, "invalid", refused["code"])
			assert.Equal(t, "$.currency", refused["param"])
			assert.Contains(t, refused["message"], "EUR")

			// Nothing was created, so the key is free for the real attempt.
			other["currency"] = "eur"
			resp, _ = acpCall(t, http.MethodPost, base, other, key)
			assert.Equal(t, http.StatusCreated, resp.StatusCode,
				"the store's own currency, in any case, is accepted")
		})

	t.Run("completing an unready session says so, not that the store "+
		"cannot be paid", func(t *testing.T) {
		resp, created := acpCall(t, http.MethodPost, base, createBody,
			uuid.NewString())
		require.Equal(t, http.StatusCreated, resp.StatusCode)

		key := uuid.NewString()
		url := base + "/" + created["id"].(string) + "/complete"
		payment := map[string]any{"payment_data": map[string]any{}}
		resp, refused := acpCall(t, http.MethodPost, url, payment, key)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "missing", refused["code"])
		assert.Contains(t, refused["message"], "missing buyer or fulfillment")

		// Nothing happened, so the key is free for the real attempt.
		resp, again := acpCall(t, http.MethodPost, url, payment, key)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "missing", again["code"])
	})

	t.Run("a state the country does not have keeps the session unready",
		func(t *testing.T) {
			resp, created := acpCall(t, http.MethodPost, base, createBody,
				uuid.NewString())
			require.Equal(t, http.StatusCreated, resp.StatusCode)

			resp, updated := acpCall(t, http.MethodPost,
				base+"/"+created["id"].(string), map[string]any{
					"buyer": buyer,
					"fulfillment_details": map[string]any{
						"name": "Μαρία Παπαδοπούλου",
						"address": map[string]any{
							"name":     "Μαρία Παπαδοπούλου",
							"line_one": "Πανεπιστημίου 12", "city": "Αθήνα",
							"state": "Bavaria", "country": "GR",
							"postal_code": "10431",
						},
					},
					"selected_fulfillment_options": []map[string]any{{
						"type": "shipping", "option_id": "acs:home_delivery",
						"item_ids": []string{"410"},
					}},
				}, uuid.NewString())
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "not_ready_for_payment", updated["status"])
			messages := updated["messages"].([]any)
			require.NotEmpty(t, messages)
			first := messages[0].(map[string]any)
			assert.Equal(t, "$.fulfillment_details.address.state",
				first["param"])
			assert.Contains(t, first["content"], "GR-14 (Αττική)")
		})

	t.Run("tokenized instruments are rejected",
		func(t *testing.T) {
			resp, created := acpCall(t, http.MethodPost, base, createBody,
				uuid.NewString())
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			sessionID := created["id"].(string)

			resp, rejected := acpCall(t, http.MethodPost,
				base+"/"+sessionID+"/complete", map[string]any{
					"payment_data": map[string]any{
						"handler_id": "stripe_spt",
						"instrument": map[string]any{
							"type": "card",
							"credential": map[string]any{
								"type": "spt", "token": "spt_123",
							},
						},
					},
				}, uuid.NewString())
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Equal(t, "unsupported", rejected["code"])
		})

	t.Run("cancel is terminal and not repeatable", func(t *testing.T) {
		resp, created := acpCall(t, http.MethodPost, base, createBody,
			uuid.NewString())
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		sessionID := created["id"].(string)

		resp, canceled := acpCall(t, http.MethodPost,
			base+"/"+sessionID+"/cancel", nil, uuid.NewString())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "canceled", canceled["status"])

		resp, _ = acpCall(t, http.MethodPost,
			base+"/"+sessionID+"/cancel", nil, uuid.NewString())
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})

	t.Run("unknown session is a protocol 404", func(t *testing.T) {
		resp, body := acpCall(t, http.MethodGet,
			base+"/"+uuid.NewString(), nil, "")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, "not_found", body["code"])
	})
}

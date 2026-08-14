//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// acpCall hits the ACP REST surface the way an agentic platform does.
func acpCall(
	t *testing.T, method, url string, body any, idemKey string,
) (*http.Response, map[string]any) {
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
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&decoded))
	return resp, decoded
}

func TestACPEndToEnd(t *testing.T) {
	gw, _ := startUCPGateway(t)
	base := gw.URL + "/acp/checkout_sessions"

	buyer := map[string]any{
		"first_name": "Μαρία", "last_name": "Παπαδοπούλου",
		"email": "maria@example.test", "phone_number": "+306912345678",
	}
	fulfillment := map[string]any{
		"name": "Μαρία Παπαδοπούλου",
		"address": map[string]any{
			"name": "Μαρία Παπαδοπούλου", "line_one": "Πανεπιστημίου 12",
			"city": "Αθήνα", "state": "", "country": "GR",
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

		resp, updated := acpCall(t, http.MethodPost, base+"/"+sessionID,
			map[string]any{
				"buyer":               buyer,
				"fulfillment_details": fulfillment,
				"selected_fulfillment_options": []map[string]any{{
					"type": "shipping", "option_id": "acs:home_delivery",
					"item_ids": []string{"410"},
				}},
			}, "")
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

	t.Run("tokenized instruments are rejected until the flag ships",
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

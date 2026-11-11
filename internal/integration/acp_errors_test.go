//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/acp"
)

// retryAfter is the whole seconds a 409 tells the platform to wait.
func retryAfter(t *testing.T, resp *http.Response) int {
	t.Helper()
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	require.NoError(t, err, "a 409 must carry Retry-After")
	return seconds
}

// Each refusal the ACP surface answers has the spec's status and code,
// and acpDo checks every error body against the spec's Error schema.
func TestACPErrorsFollowTheSpec(t *testing.T) {
	stack := startUCPGateway(t)
	base := stack.gw.URL + "/acp/checkout_sessions"
	ctx := context.Background()
	createBody := map[string]any{
		"line_items":   []map[string]any{{"id": "1", "quantity": 2}},
		"currency":     "EUR",
		"capabilities": map[string]any{},
	}
	complete := func(id, key string) (*http.Response, map[string]any) {
		return acpCall(t, http.MethodPost, base+"/"+id+"/complete",
			map[string]any{"payment_data": map[string]any{}}, key)
	}
	update := func(id, key string) (*http.Response, map[string]any) {
		return acpCall(t, http.MethodPost, base+"/"+id,
			map[string]any{"buyer": map[string]any{
				"email": "other@example.test",
			}}, key)
	}

	t.Run("API-Version is required and checked", func(t *testing.T) {
		for version, code := range map[string]string{
			"":           "missing_api_version",
			"2025-09-29": "unsupported_api_version",
		} {
			req := acpRequest(t, http.MethodPost, base, createBody,
				uuid.NewString())
			req.Header.Set("API-Version", version)
			resp, body := acpDo(t, req)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Equal(t, code, body["code"])
			assert.Equal(t, []any{acp.Version}, body["supported_versions"])
		}
	})

	t.Run("a busy session is in flight and frees the key", func(t *testing.T) {
		id := acpReady(t, base)
		lock := "ag:demostore:cs:" + id + ":lock"
		require.NoError(t, stack.rdb.Set(ctx, lock, "another request",
			10*time.Second).Err())

		key := uuid.NewString()
		resp, body := update(id, key)
		assert.Equal(t, http.StatusConflict, resp.StatusCode)
		assert.Equal(t, "idempotency_in_flight", body["code"])
		assert.Equal(t, 2, retryAfter(t, resp))
		resp, body = complete(id, uuid.NewString())
		assert.Equal(t, http.StatusConflict, resp.StatusCode)
		assert.Equal(t, "idempotency_in_flight", body["code"])
		assert.Equal(t, 2, retryAfter(t, resp))

		require.NoError(t, stack.rdb.Del(ctx, lock).Err())
		resp, _ = update(id, key)
		assert.Equal(t, http.StatusOK, resp.StatusCode,
			"the busy answer must not spend the key")
	})

	t.Run("a closed session refuses changes with 405", func(t *testing.T) {
		resp, created := acpCall(t, http.MethodPost, base, createBody,
			uuid.NewString())
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		canceled := created["id"].(string)
		resp, _ = acpCall(t, http.MethodPost, base+"/"+canceled+"/cancel",
			nil, uuid.NewString())
		require.Equal(t, http.StatusOK, resp.StatusCode)

		completed := acpReady(t, base)
		resp, _ = complete(completed, uuid.NewString())
		require.Equal(t, http.StatusOK, resp.StatusCode)

		for _, id := range []string{canceled, completed} {
			resp, body := update(id, uuid.NewString())
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
			assert.Equal(t, "conflict", body["code"])
			assert.Equal(t, http.MethodGet, resp.Header.Get("Allow"))

			resp, _ = acpCall(t, http.MethodPost, base+"/"+id+"/cancel",
				nil, uuid.NewString())
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
			assert.Contains(t, resp.Header, "Allow")
		}
		resp, body := complete(canceled, uuid.NewString())
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
		assert.Equal(t, "conflict", body["code"])
		assert.Contains(t, resp.Header, "Allow")
	})

	t.Run("a create replayed after its session expired is a new request",
		func(t *testing.T) {
			key := uuid.NewString()
			resp, first := acpCall(t, http.MethodPost, base, createBody, key)
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			require.NoError(t, stack.rdb.Del(ctx,
				"ag:demostore:cs:"+first["id"].(string)).Err())

			resp, second := acpCall(t, http.MethodPost, base, createBody, key)
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			assert.NotEqual(t, first["id"], second["id"])
			assert.Empty(t, resp.Header.Get("Idempotent-Replayed"))

			resp, replayed := acpCall(t, http.MethodPost, base, createBody,
				key)
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			assert.Equal(t, second["id"], replayed["id"])
			assert.Equal(t, "true", resp.Header.Get("Idempotent-Replayed"))
		})

	t.Run("stock gone at completion is a 400 out_of_stock", func(t *testing.T) {
		id := acpReady(t, base)
		stack.faults.stockShort.Store(true)
		t.Cleanup(func() { stack.faults.stockShort.Store(false) })
		resp, body := complete(id, uuid.NewString())
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "out_of_stock", body["code"])
		assert.Contains(t, body["message"], "Ζυμαρικά")
	})

	t.Run("an order the store refuses with a 409 is a refusal",
		func(t *testing.T) {
			id := acpReady(t, base)
			stack.faults.orderConflict.Store(true)
			t.Cleanup(func() { stack.faults.orderConflict.Store(false) })
			key := uuid.NewString()
			resp, body := complete(id, key)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Equal(t, "invalid", body["code"])
			assert.Equal(t, "The cart changed while it was ordered.",
				body["message"])

			stack.faults.orderConflict.Store(false)
			resp, done := complete(id, key)
			require.Equal(t, http.StatusOK, resp.StatusCode,
				"nothing was placed, so the key is free: %v", done)
			assert.Equal(t, "completed", done["status"])
		})

	t.Run("an unconfirmed order is pending, then unknown for good",
		func(t *testing.T) {
			id := acpReady(t, base)
			before := stack.faults.orderCreates.Load()
			stack.faults.orderCreate.Store(true)
			t.Cleanup(func() { stack.faults.orderCreate.Store(false) })
			resp, body := complete(id, uuid.NewString())
			assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
			assert.Equal(t, "order_outcome_unknown", body["code"])
			stack.faults.orderCreate.Store(false)

			// Within the completion's budget it may still be running: the
			// platform waits at most until it must have ended.
			resp, body = complete(id, uuid.NewString())
			assert.Equal(t, http.StatusConflict, resp.StatusCode)
			assert.Equal(t, "idempotency_in_flight", body["code"])
			assert.InDelta(t, 3*ucpUpstreamTimeout.Seconds(),
				retryAfter(t, resp), 2)
			resp, _ = update(id, uuid.NewString())
			assert.Equal(t, http.StatusConflict, resp.StatusCode)

			// Past it, nothing will record the outcome before the session
			// expires: no Retry-After, on any route.
			stored := stack.storedSession(t, id)
			stored.UpdatedAt = time.Now().Add(-time.Minute)
			raw, err := json.Marshal(stored)
			require.NoError(t, err)
			require.NoError(t, stack.rdb.Set(ctx, "ag:demostore:cs:"+id, raw,
				redis.KeepTTL).Err())
			for _, call := range []func() (*http.Response, map[string]any){
				func() (*http.Response, map[string]any) {
					return complete(id, uuid.NewString())
				},
				func() (*http.Response, map[string]any) {
					return update(id, uuid.NewString())
				},
				func() (*http.Response, map[string]any) {
					return acpCall(t, http.MethodPost,
						base+"/"+id+"/cancel", nil, uuid.NewString())
				},
			} {
				resp, body := call()
				assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
				assert.Equal(t, "order_outcome_unknown", body["code"])
				assert.Empty(t, resp.Header.Get("Retry-After"))
			}
			assert.EqualValues(t, 1, stack.faults.orderCreates.Load()-before,
				"no answer may send the order again")
		})
}

package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// A refusal reaches the agent with the store's code and its words, so it
// can explain it to the shopper instead of guessing.
func TestUpstreamErrCarriesTheStoresCodeAndDetail(t *testing.T) {
	cases := []struct {
		name string
		err  *django.APIError
		want *ToolError
	}{
		{
			name: "conflict",
			err: &django.APIError{Status: 409, Code: "insufficient_stock",
				Detail: "Μόνο 2 διαθέσιμα."},
			want: &ToolError{Code: "insufficient_stock",
				Detail: "Μόνο 2 διαθέσιμα."},
		},
		{
			name: "validation",
			err: &django.APIError{Status: 400, Code: "max_value",
				Detail: "quantity: Ensure this value is less than 999."},
			want: &ToolError{Code: "max_value",
				Detail: "quantity: Ensure this value is less than 999."},
		},
		{
			name: "validation without a code",
			err:  &django.APIError{Status: 400},
			want: &ToolError{Code: "invalid"},
		},
		{
			name: "throttled",
			err: &django.APIError{Status: 429, Code: "throttled",
				Detail: "Request was throttled.", RetryAfter: 42 * time.Second},
			want: &ToolError{Code: "throttled",
				Detail: "Request was throttled.", RetryAfter: 42},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := upstreamErr(fmt.Errorf("add: %w", tc.err), "not found")

			var got *ToolError
			require.ErrorAs(t, err, &got)
			assert.Equal(t, tc.want.Code, got.Code)
			assert.Equal(t, tc.want.Detail, got.Detail)
			assert.Equal(t, tc.want.RetryAfter, got.RetryAfter)
			assert.NotEmpty(t, got.Hint)
		})
	}
}

// What the agent may not know stays out: a 403 is "not found", and an
// outage says only that it is one.
func TestUpstreamErrLeaksNothingTheAgentMayNotSee(t *testing.T) {
	forbidden := upstreamErr(&django.APIError{Status: 403,
		Code: "permission_denied", Detail: "Not your order."}, "no such order")
	assert.Equal(t, &ToolError{Code: "not_found", Hint: "no such order"},
		forbidden)

	down := upstreamErr(&django.APIError{Status: 500, Code: "error",
		Detail: "relation \"cart\" does not exist"}, "not found")
	var got *ToolError
	require.ErrorAs(t, down, &got)
	assert.Equal(t, "unavailable", got.Code)
	assert.Empty(t, got.Detail)
	assert.NotContains(t, down.Error(), "relation")

	network := upstreamErr(fmt.Errorf("%w: dial tcp 10.0.0.7:8000",
		django.ErrUpstreamDown), "not found")
	assert.NotContains(t, network.Error(), "10.0.0.7")
}

// The tool result is JSON an agent can read field by field, Greek
// unescaped.
func TestToolErrorRendersAsJSON(t *testing.T) {
	err := &ToolError{Code: "insufficient_stock", Detail: "Μόνο 2 <διαθέσιμα>",
		Hint: "explain"}

	assert.JSONEq(t, `{"code":"insufficient_stock",`+
		`"detail":"Μόνο 2 <διαθέσιμα>","hint":"explain"}`, err.Error())
	assert.Contains(t, err.Error(), "Μόνο 2 <διαθέσιμα>")
}

// addDjango answers POST /cart/item with addStatus (the item, or a
// refusal) and every GET /cart with the fixture, recording the
// Idempotency-Key and the body each add sent.
func addDjango(
	t *testing.T, addStatus *int,
) (*handlers, *[]string, *[]map[string]any) {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
			"fixtures", "django", name))
		require.NoError(t, err)
		return b
	}
	cart, item := read("cart_with_items.json"), read("cart_item_created.json")
	var keys []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				_, _ = w.Write(cart)
				return
			}
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			var body map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			bodies = append(bodies, body)
			w.WriteHeader(*addStatus)
			if *addStatus < 300 {
				_, _ = w.Write(item)
				return
			}
			_, _ = w.Write([]byte(`{"detail": "busy", "code": "error"}`))
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &handlers{deps: Deps{Site: testSite, Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, nil)}}
	return h, &keys, &bodies
}

// A chat turn's add carries an idempotency key. A repeat of an add whose
// outcome is unknown reuses it, so the store answers it with the first
// outcome; once an add succeeded, an identical one is a new change.
func TestAddToCartIsIdempotentWithinATurn(t *testing.T) {
	status := http.StatusBadGateway
	h, keys, _ := addDjango(t, &status)
	ctx := WithTurn(tenant.NewContext(context.Background(), cartTenant()),
		NewTurn("8b0f6c1e-4a8d-4c8b-9a37-2f4f1d1c9e01"))
	in := AddToCartIn{CartID: fixtureCartID, ProductID: 1, Quantity: 2}

	_, _, err := h.addToCart(ctx, nil, in)
	require.Error(t, err)
	status = http.StatusCreated
	_, _, err = h.addToCart(ctx, nil, in)
	require.NoError(t, err)
	_, _, err = h.addToCart(ctx, nil, in)
	require.NoError(t, err)
	_, _, err = h.addToCart(ctx, nil,
		AddToCartIn{CartID: fixtureCartID, ProductID: 1, Quantity: 1})
	require.NoError(t, err)

	require.Len(t, *keys, 4)
	k := *keys
	assert.NotEmpty(t, k[0])
	assert.Equal(t, k[0], k[1], "a repeat of the failed add reuses its key")
	assert.NotEqual(t, k[1], k[2], "a second add after a success is new")
	assert.NotEqual(t, k[2], k[3], "another quantity is another change")

	// The same turn retried by the shopper starts from the same keys.
	retried := WithTurn(tenant.NewContext(context.Background(), cartTenant()),
		NewTurn("8b0f6c1e-4a8d-4c8b-9a37-2f4f1d1c9e01"))
	_, _, err = h.addToCart(retried, nil, in)
	require.NoError(t, err)
	assert.Equal(t, k[0], (*keys)[4])
}

// An /mcp agent has no turn: its adds carry no key.
func TestAddToCartWithoutATurnSendsNoKey(t *testing.T) {
	status := http.StatusCreated
	h, keys, _ := addDjango(t, &status)
	ctx := tenant.NewContext(context.Background(), cartTenant())

	_, _, err := h.addToCart(ctx, nil,
		AddToCartIn{CartID: fixtureCartID, ProductID: 1})

	require.NoError(t, err)
	assert.Equal(t, []string{""}, *keys)
}

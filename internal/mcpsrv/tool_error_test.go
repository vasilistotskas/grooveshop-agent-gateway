package mcpsrv

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// An add the store did not confirm may be in the cart already: the agent
// is told to check the cart, not to retry, which could add it twice.
func TestAddToCartWithAnUnknownOutcomeSaysToCheckTheCart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &handlers{deps: Deps{Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, nil)}}
	ctx := tenant.NewContext(context.Background(), cartTenant())

	_, _, err := h.addToCart(ctx, nil,
		AddToCartIn{CartID: fixtureCartID, ProductID: 1})

	assert.Equal(t, errAddUnconfirmed, err)
}

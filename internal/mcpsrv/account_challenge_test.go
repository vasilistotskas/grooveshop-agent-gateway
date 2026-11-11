package mcpsrv

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func challengeTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			Features: django.StoreFeatures{
				Catalogue: true, Cart: true, Favourites: true,
			},
		},
		Domain: "shop.example.test",
	}
}

func toolCall(name string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"` + name + `","arguments":{}}}`
}

// throughChallenge runs a request through the wrapper and reports what it
// answered, and the body the next handler read, if it was reached.
func throughChallenge(
	t *testing.T, ctx context.Context, method, body string,
) (*httptest.ResponseRecorder, string, bool) {
	t.Helper()
	var seen string
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		seen = string(b)
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	RequireLinkedAccount(next).ServeHTTP(rec, req.WithContext(ctx))
	return rec, seen, reached
}

func TestRequireLinkedAccountChallengesAnAnonymousCall(t *testing.T) {
	ctx := tenant.NewContext(context.Background(), challengeTenant())
	// One link serves every account tool this store has: loyalty is off
	// here, so it is not asked for.
	for _, tool := range []string{"my_orders", "my_favourites"} {
		rec, _, reached := throughChallenge(t, ctx, http.MethodPost,
			toolCall(tool))
		assert.False(t, reached, tool)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, tool)
		assert.Equal(t,
			`Bearer scope="favourites:read orders:read profile", `+
				`resource_metadata="https://shop.example.test`+
				`/.well-known/oauth-protected-resource/mcp"`,
			rec.Header().Get("WWW-Authenticate"))
	}
}

func TestRequireLinkedAccountLeavesTheRestOpen(t *testing.T) {
	ctx := tenant.NewContext(context.Background(), challengeTenant())
	linked := identity.NewContext(ctx, &identity.Linked{Bearer: "token"})
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		method string
		body   string
	}{
		{"a public tool", ctx, http.MethodPost, toolCall("search_products")},
		{"listing the tools", ctx, http.MethodPost,
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"a linked caller", linked, http.MethodPost, toolCall("my_orders")},
		{"a body only the handler can judge", ctx, http.MethodPost, "not json"},
		{"another method", ctx, http.MethodGet, ""},
		// Loyalty is off for this store: the tool does not exist here, so
		// linking an account would not bring it back.
		{"a tool the store does not serve", ctx, http.MethodPost,
			toolCall("my_loyalty_points")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, seen, reached := throughChallenge(
				t, tc.ctx, tc.method, tc.body)
			assert.True(t, reached)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.body, seen, "the handler reads the whole body")
		})
	}
}

func TestRequireLinkedAccountBoundsTheBody(t *testing.T) {
	ctx := tenant.NewContext(context.Background(), challengeTenant())
	rec, _, reached := throughChallenge(t, ctx, http.MethodPost,
		strings.Repeat("x", maxRequestBodyBytes+1))
	assert.False(t, reached)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// A body that could not be read is not one that was too large.
func TestRequireLinkedAccountRefusesAnUnreadableBody(t *testing.T) {
	ctx := tenant.NewContext(context.Background(), challengeTenant())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp",
		iotest.ErrReader(io.ErrUnexpectedEOF))
	rec := httptest.NewRecorder()
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unread body must not reach the handler")
	})

	RequireLinkedAccount(next).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEveryAccountToolHasItsToolDefinition(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range definitions() {
		names[tool.def.Name] = true
	}
	for name := range accountScopes {
		assert.True(t, names[name], name)
	}
}

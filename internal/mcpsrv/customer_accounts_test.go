package mcpsrv

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

var accountTools = []string{"my_orders", "my_loyalty_points", "my_favourites"}

// noAccounts is every feature but customer accounts. Django also reads
// the features that need an account as off then; they are left on here
// so the switch alone is what takes the account tools away.
func noAccounts() django.StoreFeatures {
	f := EveryFeature
	f.CustomerAccounts = false
	return f
}

// A store without customer accounts serves no account tool, on /mcp, to
// the chat bridge's filter or in discovery, and nothing else goes.
func TestAStoreWithoutCustomerAccountsServesNoAccountTool(t *testing.T) {
	every := toolsListed(t, EveryFeature)
	listed := toolsListed(t, noAccounts())

	for _, name := range accountTools {
		assert.Contains(t, every, name)
		assert.NotContains(t, listed, name)
		assert.False(t, Offered(name, noAccounts()), name)
	}
	assert.Len(t, listed, len(every)-len(accountTools))

	tn := cardTenant()
	tn.Features = noAccounts()
	entry := firstEntry(t, decode(t, serve(t, ARDHandler(), tn,
		http.MethodGet, nil)))
	capabilities, ok := entry["capabilities"].([]any)
	require.True(t, ok)
	for _, name := range accountTools {
		assert.NotContains(t, capabilities, name)
	}
	assert.Len(t, capabilities, len(listed))
}

// callMCP posts a tools/call of name, anonymously, through the account
// challenge to /mcp's handler for a store with features.
func callMCP(
	t *testing.T, features django.StoreFeatures, name string,
) *httptest.ResponseRecorder {
	t.Helper()
	store := cartTenant()
	store.Features = features
	h := RequireLinkedAccount(Handler(Deps{Site: testSite, Version: "test",
		Log: slog.New(slog.DiscardHandler)}))
	req := httptest.NewRequestWithContext(
		tenant.NewContext(t.Context(), store),
		http.MethodPost, "/mcp", strings.NewReader(toolCall(name)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// rpcError is the JSON-RPC error a response carries.
func rpcError(t *testing.T, rec *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	var resp struct {
		Error json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
		rec.Body.String())
	require.NotEmpty(t, resp.Error, rec.Body.String())
	return resp.Error
}

// Without customer accounts an anonymous call to an account tool is not
// challenged: the tool does not exist there, so the SDK answers it as
// it answers any tool the store is not served. With them it is
// challenged, as before.
func TestAnAccountToolWithoutCustomerAccountsIsAnUnknownTool(t *testing.T) {
	unknown := callMCP(t, noAccounts(), "no_such_tool")
	want := strings.Replace(string(rpcError(t, unknown)),
		"no_such_tool", "%s", 1)

	for _, name := range accountTools {
		t.Run(name, func(t *testing.T) {
			rec := callMCP(t, noAccounts(), name)

			assert.Equal(t, unknown.Code, rec.Code, rec.Body.String())
			assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
			assert.JSONEq(t, strings.Replace(want, "%s", name, 1),
				string(rpcError(t, rec)))

			linkable := callMCP(t, EveryFeature, name)
			assert.Equal(t, http.StatusUnauthorized, linkable.Code)
			assert.Contains(t, linkable.Header().Get("WWW-Authenticate"),
				"/.well-known/oauth-protected-resource/mcp")
		})
	}
}

// An order that cannot be opened is not answered "sign in" by a store
// that has no sign-in: Django answers 404 for it there.
func TestAnOrderThatCannotBeOpenedNamesNoSignInWithoutAccounts(t *testing.T) {
	assert.Contains(t, orderNotOpened(EveryFeature), "sign in")
	without := orderNotOpened(noAccounts())
	assert.NotContains(t, without, "sign")
	assert.Contains(t, without, "contact the store")
}

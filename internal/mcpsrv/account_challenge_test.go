package mcpsrv

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

func toolCall(name string) string { return numberedCall("1", name) }

// numberedCall is a tools/call of name with the JSON-RPC id id.
func numberedCall(id, name string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call",` +
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

// Bodies the MCP SDK would run an account tool from, which an anonymous
// caller must not reach it with.
var accountCallBodies = map[string]string{
	"a batch of one call": `[` + toolCall("my_orders") + `]`,
	"a batch with the call second": `[` +
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"},` +
		numberedCall("2", "my_favourites") + `]`,
	"a batch with a public call first": `[` + toolCall("search_products") +
		`,` + numberedCall("2", "my_orders") + `]`,
	"a body naming the tool twice in two cases": `{"jsonrpc":"2.0",` +
		`"id":1,"method":"tools/call","params":{"name":"my_orders",` +
		`"Name":"search_products","arguments":{}}}`,
	"a body naming the method twice in two cases": `{"jsonrpc":"2.0",` +
		`"id":1,"method":"tools/call","Method":"tools/list",` +
		`"params":{"name":"my_orders","arguments":{}}}`,
}

func TestRequireLinkedAccountChallengesEveryWayToCallAnAccountTool(
	t *testing.T,
) {
	ctx := tenant.NewContext(context.Background(), challengeTenant())
	for name, body := range accountCallBodies {
		t.Run(name, func(t *testing.T) {
			rec, _, reached := throughChallenge(t, ctx, http.MethodPost, body)
			assert.False(t, reached)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Contains(t, rec.Header().Get("WWW-Authenticate"),
				`scope="favourites:read orders:read profile"`)
		})
	}
}

func TestRequireLinkedAccountLeavesOtherBatchesOpen(t *testing.T) {
	ctx := tenant.NewContext(context.Background(), challengeTenant())
	for name, body := range map[string]string{
		"public calls": `[` + toolCall("search_products") + `,` +
			toolCall("list_categories") + `]`,
		"a call the SDK reads as a public tool": `{"jsonrpc":"2.0","id":1,` +
			`"method":"tools/call","params":{"Name":"my_orders",` +
			`"name":"search_products","arguments":{}}}`,
		"another method naming the tool": `{"jsonrpc":"2.0","id":1,` +
			`"method":"prompts/get","params":{"name":"my_orders"}}`,
		"an empty batch":             `[]`,
		"a batch with a non-message": `[` + toolCall("my_orders") + `,7]`,
	} {
		t.Run(name, func(t *testing.T) {
			rec, seen, reached := throughChallenge(t, ctx, http.MethodPost,
				body)
			assert.True(t, reached)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, body, seen)
		})
	}
}

// calledTools reads a body as the SDK does: for every body here, it names
// the tools a real streamable HTTP handler runs from it, no more and no
// fewer.
func TestCalledToolsReadsABodyAsTheSDKDoes(t *testing.T) {
	bodies := map[string]string{
		"one call": toolCall("my_orders"),
		"no call":  `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		"not json": "not json",
		"a batch of two calls": `[` + toolCall("search_products") + `,` +
			numberedCall("2", "my_favourites") + `]`,
		"the name in another case only": `{"jsonrpc":"2.0","id":1,` +
			`"method":"tools/call","params":{"NAME":"my_orders"}}`,
		"the name twice": `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
			`"params":{"name":"my_orders","name":"search_products"}}`,
		"trailing data": toolCall("my_orders") + ` ` + toolCall("my_favourites"),
		"no version": `{"id":1,"method":"tools/call",` +
			`"params":{"name":"my_orders"}}`,
		"null params": `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
			`"params":null}`,
	}
	maps.Copy(bodies, accountCallBodies)

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			assert.ElementsMatch(t, toolsTheSDKRuns(t, body),
				probeTools(calledTools([]byte(body))))
		})
	}
}

// A batch whose calls share an id: the SDK runs the first only, and every
// call is read here, so the challenge covers what the SDK runs.
func TestCalledToolsCoversABatchWithOneIDTwice(t *testing.T) {
	body := `[` + toolCall("search_products") + `,` +
		toolCall("my_orders") + `]`

	ran := toolsTheSDKRuns(t, body)

	assert.Equal(t, []string{"search_products"}, ran)
	assert.Subset(t, calledTools([]byte(body)), ran)
	assert.Contains(t, calledTools([]byte(body)), "my_orders")
}

// probeTools keeps the names toolsTheSDKRuns' server has a tool for.
func probeTools(names []string) []string {
	return slices.DeleteFunc(names, func(name string) bool {
		return !slices.Contains(probeToolNames, name)
	})
}

var probeToolNames = []string{"my_orders", "my_favourites", "search_products"}

// toolsTheSDKRuns posts body, without MCP-Protocol-Version as the probe
// did, to a stateless streamable HTTP handler built as Handler builds
// /mcp's, and lists the tools it ran.
func toolsTheSDKRuns(t *testing.T, body string) []string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "probe"}, nil)
	var mu sync.Mutex
	var ran []string
	for _, name := range probeToolNames {
		mcp.AddTool(srv, &mcp.Tool{Name: name}, func(
			context.Context, *mcp.CallToolRequest, struct{},
		) (*mcp.CallToolResult, struct{}, error) {
			mu.Lock()
			ran = append(ran, name)
			mu.Unlock()
			return nil, struct{}{}, nil
		})
	}
	h := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	h.ServeHTTP(httptest.NewRecorder(), req)
	mu.Lock()
	defer mu.Unlock()
	return ran
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

// The account tools are read off the toolset, each with the scope its
// description names, and none of them is the chat's.
func TestAccountToolsCarryTheirScopes(t *testing.T) {
	assert.Equal(t, map[string]string{
		"my_orders":         "orders:read",
		"my_loyalty_points": "loyalty:read",
		"my_favourites":     "favourites:read",
	}, accountScopes())
	for name, scope := range accountScopes() {
		tool := byName()[name]
		assert.Contains(t, tool.def.Description, scope, name)
		assert.False(t, tool.servedOn(chatSurface), name)
		assert.True(t, tool.servedOn(mcpSurface), name)
	}
}

package adminmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// bareToken is a staff token whose /agent/staff/me names no scopes.
const bareToken = "bare-staff-token"

// emptyStoreDjango answers /agent/staff/* as a store with nothing to
// list: goodToken is granted every scope, bareToken's profile carries no
// scopes at all, and a list inside an object is left out, as a missing
// field decodes the same as Django's null.
func emptyStoreDjango(t *testing.T) *django.Client {
	t.Helper()
	report := `{"start":"2026-10-01","end":"2026-10-31","bucket":null,` +
		`"currency":"EUR","total":{"period":null}}`
	answers := map[string]string{
		"orders": `[]`, "orders/7": `{"id":7,"uuid":"u","total":0}`,
		"sales": report, "profit": report, "products": `[]`,
		"customers": `[]`, "reviews": `[]`, "messages": `[]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+goodToken && auth != "Bearer "+bareToken {
				w.Header().Set("WWW-Authenticate", `Bearer realm="agent-staff"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			path := strings.TrimPrefix(r.URL.Path, "/api/v1/agent/staff/")
			if path == "me" {
				profile := map[string]any{"email": "owner@acme.test",
					"role": "owner", "roleDisplay": "Owner",
					"storeName": "Acme", "storeDomain": "shop.acme.test"}
				if auth == "Bearer "+goodToken {
					scopes := []map[string]any{}
					for _, s := range ReadScopes {
						scopes = append(scopes, map[string]any{"scope": s,
							"description": s, "granted": true, "allowed": true})
					}
					profile["scopes"] = scopes
				}
				_ = json.NewEncoder(w).Encode(profile)
				return
			}
			body, ok := answers[path]
			if !ok {
				t.Errorf("unexpected call %s", path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.acme.test", "secret",
		5*time.Second, quietLog(), obs.NewMetrics())
}

// nullLists lists the paths where value has null for a list its schema
// declares.
func nullLists(path string, schema map[string]any, value any) []string {
	types, _ := schema["type"].([]any)
	if t, ok := schema["type"].(string); ok {
		types = []any{t}
	}
	if value == nil {
		if slices.Contains(types, any("array")) {
			return []string{path}
		}
		return nil
	}
	var out []string
	switch v := value.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for k, member := range v {
			sub, _ := props[k].(map[string]any)
			out = append(out, nullLists(path+"."+k, sub, member)...)
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		for i, member := range v {
			out = append(out,
				nullLists(fmt.Sprintf("%s[%d]", path, i), items, member)...)
		}
	}
	return out
}

func connectAs(t *testing.T, dj *django.Client, token string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(stack(dj, acme()))
	t.Cleanup(srv.Close)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{
			Endpoint:   srv.URL + Path,
			HTTPClient: &http.Client{Transport: bearer{token}},
		}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// A list a tool has nothing for is [], never null, as on /mcp: an app
// iterating it must not have to guard against a missing list. Each tool
// is called once against a store with nothing to list, and whoami again
// for a connection whose profile names no scopes. A new tool fails here
// until it is called too.
func TestToolOutputsListNothingAsEmptyArrays(t *testing.T) {
	args := map[string]map[string]any{
		"whoami": {}, "list_orders": {}, "get_order": {"orderId": 7},
		"sales_report": {}, "profit_report": {}, "list_products": {},
		"find_customers": {}, "list_reviews": {}, "list_messages": {},
	}
	dj := emptyStoreDjango(t)
	granted := connectAs(t, dj, goodToken)
	bare := connectAs(t, dj, bareToken)
	listed, err := granted.ListTools(t.Context(), nil)
	require.NoError(t, err)

	check := func(
		t *testing.T, session *mcp.ClientSession, tool *mcp.Tool,
	) {
		in, ok := args[tool.Name]
		require.True(t, ok, "call the new tool %s here", tool.Name)
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: tool.Name, Arguments: in,
		})
		require.NoError(t, err)
		require.False(t, res.IsError, "%v", res.Content)
		raw, err := json.Marshal(res.StructuredContent)
		require.NoError(t, err)
		var out any
		require.NoError(t, json.Unmarshal(raw, &out))
		schemaJSON, err := json.Marshal(tool.OutputSchema)
		require.NoError(t, err)
		var schema map[string]any
		require.NoError(t, json.Unmarshal(schemaJSON, &schema))
		assert.Empty(t, nullLists("$", schema, out), "%s", raw)
	}
	for _, tool := range listed.Tools {
		t.Run(tool.Name, func(t *testing.T) { check(t, granted, tool) })
		if tool.Name == "whoami" {
			t.Run("whoami without scopes", func(t *testing.T) {
				check(t, bare, tool)
			})
		}
	}
}

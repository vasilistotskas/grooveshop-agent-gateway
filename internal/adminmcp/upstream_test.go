package adminmcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// ordersDjango verifies goodToken as a person whose role opens every
// read scope but those in barred, and answers /agent/staff/orders with
// status and body. It counts the order reads.
func ordersDjango(
	t *testing.T, barred []string, status int, body string,
) (*django.Client, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/agent/staff/me":
				scopes := []map[string]any{}
				for _, s := range ReadScopes {
					allowed := true
					for _, b := range barred {
						allowed = allowed && b != s
					}
					scopes = append(scopes, map[string]any{
						"scope": s, "description": s,
						"granted": true, "allowed": allowed,
					})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"email": "help@acme.test", "role": "support",
					"roleDisplay": "Support", "storeName": "Acme",
					"storeDomain": "shop.acme.test", "scopes": scopes,
				})
			case "/api/v1/agent/staff/orders":
				reads.Add(1)
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "9")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			default:
				t.Errorf("unexpected call to %s", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.acme.test", "secret",
		time.Second, quietLog(), obs.NewMetrics()), &reads
}

// toolError is the ToolError a failed call answered with.
func toolError(t *testing.T, res *mcp.CallToolResult) mcptool.ToolError {
	t.Helper()
	require.True(t, res.IsError)
	var e mcptool.ToolError
	require.NoError(t, json.Unmarshal(
		[]byte(res.Content[0].(*mcp.TextContent).Text), &e))
	return e
}

// A scope the app was granted and the person's role does not open is
// refused before the store is asked, naming the role: the person needs
// to know it is their role, not the app.
func TestARoleThatDoesNotOpenTheScopeIsRefusedNamingIt(t *testing.T) {
	dj, reads := ordersDjango(t, []string{ScopeOrders}, http.StatusOK, `[]`)
	session := connect(t, stack(dj, acme()))

	res, err := session.CallTool(t.Context(),
		&mcp.CallToolParams{Name: "list_orders"})
	require.NoError(t, err)

	e := toolError(t, res)
	assert.Equal(t, mcptool.CodeNotAllowed, e.Code)
	assert.Contains(t, e.Hint, "(Support)")
	assert.Zero(t, reads.Load(), "the store was asked")
}

// What the store answers mid-call, after the bearer was verified, is
// reported by what the person can do about it.
func TestAStoreFailureMidCallIsReportedByItsKind(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   mcptool.ToolError
		reads  int32
	}{
		{
			name: "403", status: http.StatusForbidden,
			body:  `{"detail":"You do not have permission."}`,
			want:  mcptool.ToolError{Code: mcptool.CodeNotAllowed},
			reads: 1,
		},
		{
			name: "401", status: http.StatusUnauthorized,
			body:  `{"detail":"Invalid token."}`,
			want:  mcptool.ToolError{Code: mcptool.CodeUnauthorized},
			reads: 1,
		},
		{
			name: "400 with its detail", status: http.StatusBadRequest,
			body: `{"detail":"End must not be before start.",` +
				`"code":"invalid"}`,
			want: mcptool.ToolError{Code: mcptool.CodeInvalid,
				Detail: "End must not be before start."},
			reads: 1,
		},
		{
			name: "429", status: http.StatusTooManyRequests,
			body: `{"detail":"Request was throttled."}`,
			want: mcptool.ToolError{Code: mcptool.CodeThrottled,
				RetryAfter: 9},
			reads: 1,
		},
		{
			// A GET is asked three times before it is an outage.
			name: "500", status: http.StatusInternalServerError,
			body: `{"detail":"Traceback (most recent call last)"}`,
			want: mcptool.ToolError{Code: mcptool.CodeUnavailable,
				RetryAfter: mcptool.UnavailableRetryAfter},
			reads: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dj, reads := ordersDjango(t, nil, tc.status, tc.body)
			session := connect(t, stack(dj, acme()))

			res, err := session.CallTool(t.Context(),
				&mcp.CallToolParams{Name: "list_orders"})
			require.NoError(t, err)

			e := toolError(t, res)
			assert.NotEmpty(t, e.Hint)
			e.Hint = ""
			assert.Equal(t, tc.want, e)
			assert.Equal(t, tc.reads, reads.Load())
			assert.NotContains(t, res.Content[0].(*mcp.TextContent).Text,
				"Traceback", "the store's own text of an outage")
		})
	}
}

func cachedServer(c *serverCache, tn *tenant.Tenant) *mcp.Server {
	r := httptest.NewRequest(http.MethodPost, Path, nil)
	if tn != nil {
		r = r.WithContext(tenant.NewContext(r.Context(), tn))
	}
	return c.forRequest(r)
}

// One server per store and the name it announces: a store's calls share
// one, another store gets its own, a store that renames itself gets a new
// one, and the cache starts over once it holds maxCachedServers.
func TestTheServerCacheKeepsOnePerStoreIdentity(t *testing.T) {
	c := &serverCache{deps: Deps{Log: quietLog(), Version: "test"},
		servers: map[string]*mcp.Server{}}

	assert.Nil(t, cachedServer(c, nil), "a request without its store")

	first := cachedServer(c, acme())
	require.NotNil(t, first)
	assert.Same(t, first, cachedServer(c, acme()))

	alpha := acme()
	alpha.SchemaName = "alpha"
	assert.NotSame(t, first, cachedServer(c, alpha))

	renamed := acme()
	renamed.StoreName = "Acme Outlet"
	assert.NotSame(t, first, cachedServer(c, renamed))
	moved := acme()
	moved.PrimaryDomain = "acme-outlet.test"
	assert.NotSame(t, first, cachedServer(c, moved))
	assert.Len(t, c.servers, 4)

	for i := len(c.servers); i < maxCachedServers; i++ {
		other := acme()
		other.SchemaName = fmt.Sprintf("store%d", i)
		cachedServer(c, other)
	}
	require.Len(t, c.servers, maxCachedServers)
	assert.Same(t, first, cachedServer(c, acme()), "a hit evicts nothing")
	newcomer := acme()
	newcomer.SchemaName = "newcomer"
	cachedServer(c, newcomer)
	assert.Len(t, c.servers, 1, "the full cache started over")
	assert.NotSame(t, first, cachedServer(c, acme()))
}

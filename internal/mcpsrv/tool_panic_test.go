package mcpsrv

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// A tool handler that panics is answered with a tool error, and the
// process lives on: the SDK runs a handler on a goroutine of its own,
// out of reach of the HTTP recover. Here the store's client is missing,
// so the first call to it dereferences nil.
func TestAPanickingToolIsAnsweredOverTheStatelessHandler(t *testing.T) {
	lines := &toolLines{}
	store := cartTenant()
	store.Features = EveryFeature
	h := Handler(Deps{Site: testSite, Version: "test",
		Log: slog.New(lines)})
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(
				tenant.NewContext(r.Context(), store)))
		}))
	t.Cleanup(srv.Close)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(t.Context(),
			&mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	res, err := session.CallTool(t.Context(),
		&mcp.CallToolParams{Name: "list_categories"})

	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text,
		`"code":"unavailable"`)
	lines.mu.Lock()
	defer lines.mu.Unlock()
	assert.Equal(t, [][2]string{{"mcp", "list_categories"}}, lines.lines)
	assert.Equal(t, []string{"panic"}, lines.outcomes)
}

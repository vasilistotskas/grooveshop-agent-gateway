package mcpsrv

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolLines keeps the server and tool, and the outcome, of every "tool
// call" line.
type toolLines struct {
	mu       sync.Mutex
	lines    [][2]string
	outcomes []string
}

func (l *toolLines) Enabled(context.Context, slog.Level) bool { return true }

func (l *toolLines) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "tool call" {
		return nil
	}
	var line [2]string
	var outcome string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "server":
			line[0] = a.Value.String()
		case "tool":
			line[1] = a.Value.String()
		case "outcome":
			outcome = a.Value.String()
		}
		return true
	})
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.outcomes = append(l.outcomes, outcome)
	l.mu.Unlock()
	return nil
}

func (l *toolLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *toolLines) WithGroup(string) slog.Handler      { return l }

// Every tool call on /mcp and in the chat is logged once, naming which
// of them served it, as /mcp/admin's are.
func TestEveryToolCallIsLoggedWithItsServer(t *testing.T) {
	for _, tc := range []struct {
		server string
		build  func(Deps) *mcp.Server
	}{
		{"mcp", func(d Deps) *mcp.Server {
			return NewServer(d, Identity{Name: "test.acme/store"},
				EveryFeature)
		}},
		{"chat", NewChatServer},
	} {
		t.Run(tc.server, func(t *testing.T) {
			lines := &toolLines{}
			srv := tc.build(Deps{Version: "test", Log: slog.New(lines)})
			serverT, clientT := mcp.NewInMemoryTransports()
			_, err := srv.Connect(t.Context(), serverT, nil)
			require.NoError(t, err)
			session, err := mcp.NewClient(
				&mcp.Implementation{Name: "test"}, nil,
			).Connect(t.Context(), clientT, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = session.Close() })

			// No store on the context: each tool fails at once, and is
			// logged all the same. get_cart is registered per surface
			// (cartToolOf).
			for _, call := range []*mcp.CallToolParams{
				{Name: "my_orders"},
				{Name: "get_cart", Arguments: map[string]any{"cartId": "c"}},
			} {
				_, err = session.CallTool(t.Context(), call)
				require.NoError(t, err)
			}

			lines.mu.Lock()
			defer lines.mu.Unlock()
			assert.Equal(t, [][2]string{
				{tc.server, "my_orders"}, {tc.server, "get_cart"},
			}, lines.lines)
		})
	}
}

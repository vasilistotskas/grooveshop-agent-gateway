package mcptool

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tool result is JSON an agent can read field by field, Greek
// unescaped.
func TestToolErrorRendersAsJSON(t *testing.T) {
	err := &ToolError{Code: "insufficient_stock", Detail: "Μόνο 2 <διαθέσιμα>",
		Hint: "explain"}

	assert.JSONEq(t, `{"code":"insufficient_stock",`+
		`"detail":"Μόνο 2 <διαθέσιμα>","hint":"explain"}`, err.Error())
	assert.Contains(t, err.Error(), "Μόνο 2 <διαθέσιμα>")
}

// records keeps every record logged through it, with its attributes.
type records struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }

func (r *records) Handle(_ context.Context, rec slog.Record) error {
	line := map[string]any{"msg": rec.Message, "level": rec.Level}
	rec.Attrs(func(a slog.Attr) bool {
		line[a.Key] = a.Value.Any()
		return true
	})
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
	return nil
}

func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

// Every call is one info line naming the server, the tool and the
// outcome: ok, the ToolError's code, or "error"; never the arguments.
func TestAddLogsOneLinePerCall(t *testing.T) {
	log := &records{}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	failures := map[string]error{
		"works":   nil,
		"refuses": &ToolError{Code: CodeNotGranted, Hint: "re-link"},
		"breaks":  errors.New("boom"),
	}
	for name, failure := range failures {
		Add(srv, slog.New(log), "mcp_admin", &mcp.Tool{Name: name}, func(
			context.Context, *mcp.CallToolRequest, struct {
				Secret string `json:"secret"`
			},
		) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, failure
		})
	}
	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := srv.Connect(t.Context(), serverT, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(t.Context(), clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	for _, name := range []string{"works", "refuses", "breaks"} {
		_, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: name, Arguments: map[string]any{"secret": "s3cr3t"},
		})
		require.NoError(t, err)
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	require.Len(t, log.lines, 3)
	for i, want := range []struct{ tool, outcome string }{
		{"works", "ok"}, {"refuses", CodeNotGranted}, {"breaks", "error"},
	} {
		line := log.lines[i]
		assert.Equal(t, "tool call", line["msg"])
		assert.Equal(t, slog.LevelInfo, line["level"])
		assert.Equal(t, "mcp_admin", line["server"])
		assert.Equal(t, want.tool, line["tool"])
		assert.Equal(t, want.outcome, line["outcome"])
		assert.Contains(t, line, "duration_ms")
		assert.NotContains(t, line, "secret")
	}
}

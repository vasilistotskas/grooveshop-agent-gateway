package mcptool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
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

// output is a log destination the SDK's handler goroutines and the test
// may share.
type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

// lines decodes every JSON line written so far.
func (o *output) lines(t *testing.T) (string, []map[string]any) {
	t.Helper()
	o.mu.Lock()
	raw := o.buf.String()
	o.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(raw) {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		out = append(out, m)
	}
	return raw, out
}

const (
	argumentValue = "argument-value-7f3a"
	resultValue   = "result-value-9c1e"
)

type secretIn struct {
	Secret string `json:"secret"`
}

type secretOut struct {
	Echo string `json:"echo"`
}

// serve registers tools on a server, connects a client to it, and
// returns the client session.
func serve(t *testing.T, register func(*mcp.Server)) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	register(srv)
	serverT, clientT := mcp.NewInMemoryTransports()
	_, err := srv.Connect(t.Context(), serverT, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(t.Context(), clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// Every call is one info line with exactly the server, the tool, the
// outcome (ok, the ToolError's code, or error) and the duration; the
// arguments and the result appear nowhere in what is logged.
func TestAddLogsOneLinePerCall(t *testing.T) {
	out := &output{}
	log := slog.New(slog.NewJSONHandler(out, nil))
	failures := map[string]error{
		"works":   nil,
		"refuses": &ToolError{Code: CodeNotGranted, Hint: "re-link"},
		"breaks":  errors.New("boom"),
	}
	session := serve(t, func(srv *mcp.Server) {
		for name, failure := range failures {
			Add(srv, log, ServerAdmin, &mcp.Tool{Name: name}, func(
				_ context.Context, _ *mcp.CallToolRequest, in secretIn,
			) (*mcp.CallToolResult, secretOut, error) {
				return nil, secretOut{Echo: resultValue + in.Secret}, failure
			})
		}
	})

	for _, name := range []string{"works", "refuses", "breaks"} {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: name, Arguments: map[string]any{"secret": argumentValue},
		})
		require.NoError(t, err)
		if name == "works" {
			require.False(t, res.IsError)
		}
	}

	raw, lines := out.lines(t)
	require.Len(t, lines, 3)
	for i, want := range []struct{ tool, outcome string }{
		{"works", "ok"}, {"refuses", CodeNotGranted}, {"breaks", "error"},
	} {
		line := lines[i]
		assert.Equal(t, []string{
			"duration_ms", "level", "msg", "outcome", "server", "time", "tool",
		}, slices.Sorted(maps.Keys(line)))
		assert.Equal(t, "tool call", line["msg"])
		assert.Equal(t, "INFO", line["level"])
		assert.Equal(t, ServerAdmin, line["server"])
		assert.Equal(t, want.tool, line["tool"])
		assert.Equal(t, want.outcome, line["outcome"])
	}
	assert.NotContains(t, raw, argumentValue)
	assert.NotContains(t, raw, resultValue)
}

// A panicking handler is answered as unavailable, logged with its stack
// and the outcome panic, and the process lives on: the SDK runs a
// handler on a goroutine no recover covers.
func TestAddRecoversAPanickingHandler(t *testing.T) {
	out := &output{}
	log := slog.New(slog.NewJSONHandler(out, nil))
	session := serve(t, func(srv *mcp.Server) {
		Add(srv, log, ServerMCP, &mcp.Tool{Name: "explodes"}, func(
			context.Context, *mcp.CallToolRequest, secretIn,
		) (*mcp.CallToolResult, secretOut, error) {
			var nothing *secretOut
			return nil, *nothing, nil
		})
	})

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "explodes", Arguments: map[string]any{"secret": argumentValue},
	})

	require.NoError(t, err)
	require.True(t, res.IsError)
	var answered ToolError
	require.NoError(t, json.Unmarshal(
		[]byte(res.Content[0].(*mcp.TextContent).Text), &answered))
	assert.Equal(t, CodeUnavailable, answered.Code)
	assert.Equal(t, UnavailableRetryAfter, answered.RetryAfter)
	assert.NotContains(t, answered.Hint, "nil pointer")

	raw, lines := out.lines(t)
	require.Len(t, lines, 2)
	assert.Equal(t, "panic recovered", lines[0]["msg"])
	assert.Equal(t, "mcp tool explodes", lines[0]["op"])
	assert.Contains(t, lines[0]["stack"], "mcptool")
	assert.Equal(t, "tool call", lines[1]["msg"])
	assert.Equal(t, OutcomePanic, lines[1]["outcome"])
	assert.NotContains(t, raw, argumentValue)
}

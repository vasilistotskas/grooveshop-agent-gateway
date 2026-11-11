package mcptool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
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

// toolCalls is gateway_tool_calls_total by "server tool outcome".
func toolCalls(t *testing.T, m *obs.Metrics) map[string]float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "gateway_tool_calls_total" {
			continue
		}
		for _, metric := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			out[labels["server"]+" "+labels["tool"]+" "+
				labels["outcome"]] = metric.GetCounter().GetValue()
		}
	}
	return out
}

// Every call is one info line with exactly the server, the tool, the
// outcome (ok, the ToolError's code, or error) and the duration; the
// arguments and the result appear nowhere in what is logged. Each is
// counted too, a code the store gave as refused: the counter's values
// are the gateway's own.
func TestAddLogsAndCountsOneLinePerCall(t *testing.T) {
	out := &output{}
	metrics := obs.NewMetrics()
	rec := Recorder{Server: ServerAdmin,
		Log: slog.New(slog.NewJSONHandler(out, nil)), Metrics: metrics}
	failures := map[string]error{
		"works":   nil,
		"refuses": &ToolError{Code: CodeNotGranted, Hint: "re-link"},
		"store":   &ToolError{Code: "insufficient_stock", Hint: "explain"},
		"breaks":  errors.New("boom"),
	}
	session := serve(t, func(srv *mcp.Server) {
		for name, failure := range failures {
			Add(srv, rec, &mcp.Tool{Name: name}, func(
				_ context.Context, _ *mcp.CallToolRequest, in secretIn,
			) (*mcp.CallToolResult, secretOut, error) {
				return nil, secretOut{Echo: resultValue + in.Secret}, failure
			})
		}
	})

	for _, name := range []string{"works", "refuses", "store", "breaks"} {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: name, Arguments: map[string]any{"secret": argumentValue},
		})
		require.NoError(t, err)
		if name == "works" {
			require.False(t, res.IsError)
		}
	}

	assert.Equal(t, map[string]float64{
		"mcp_admin works ok":                  1,
		"mcp_admin refuses " + CodeNotGranted: 1,
		"mcp_admin store refused":             1,
		"mcp_admin breaks error":              1,
	}, toolCalls(t, metrics))
	raw, lines := out.lines(t)
	require.Len(t, lines, 4)
	for i, want := range []struct{ tool, outcome string }{
		{"works", "ok"}, {"refuses", CodeNotGranted},
		{"store", "insufficient_stock"}, {"breaks", "error"},
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
	metrics := obs.NewMetrics()
	rec := Recorder{Server: ServerMCP,
		Log: slog.New(slog.NewJSONHandler(out, nil)), Metrics: metrics}
	session := serve(t, func(srv *mcp.Server) {
		Add(srv, rec, &mcp.Tool{Name: "explodes"}, func(
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
	assert.Equal(t, map[string]float64{"mcp explodes panic": 1},
		toolCalls(t, metrics))
}

// Every Code constant is counted as itself: a code added to the list
// above gatewayCodes and not to it would be counted as refused.
func TestEveryGatewayCodeIsCountedAsItself(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "mcptool.go", nil, 0)
	require.NoError(t, err)
	var codes []string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if strings.HasPrefix(name.Name, "Code") {
				lit := spec.Values[i].(*ast.BasicLit)
				code, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				codes = append(codes, code)
			}
		}
		return true
	})

	require.NotEmpty(t, codes)
	assert.ElementsMatch(t, codes, gatewayCodes)
	for _, code := range codes {
		assert.Equal(t, code, counted(code))
	}
}

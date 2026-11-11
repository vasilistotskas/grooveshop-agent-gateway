package mcpsrv

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// nested is a number wrapped in depth levels of alternating arrays and
// objects.
func nested(depth int) string {
	var open, closing strings.Builder
	for i := range depth {
		if i%2 == 0 {
			open.WriteString(`[`)
			closing.WriteString(`]`)
		} else {
			open.WriteString(`{"k":`)
			closing.WriteString(`}`)
		}
	}
	c := []byte(closing.String())
	for i, j := 0, len(c)-1; i < j; i, j = i+1, j-1 {
		c[i], c[j] = c[j], c[i]
	}
	return open.String() + "1" + string(c)
}

// deepCall is a call of my_orders whose whole body nests depth levels: the
// levels the message and its params take, and the rest in _meta, which
// every tool call may carry.
func deepCall(depth int) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
		`{"name":"my_orders","arguments":{},"_meta":{"v":` +
		nested(depth-3) + `}}}`
}

// depthBodies are bodies on both sides of the SDK's nesting bound, and
// the inputs its scan treats specially: brackets and escaped quotes in
// strings, closings with nothing open, a backslash at the end, and text
// that is not JSON.
var depthBodies = map[string]string{
	"depth 999":                    nested(sdkMaxDepth - 1),
	"depth 1000":                   nested(sdkMaxDepth),
	"depth 1001":                   nested(sdkMaxDepth + 1),
	"a call at depth 1000":         deepCall(sdkMaxDepth),
	"a call at depth 1001":         deepCall(sdkMaxDepth + 1),
	"a batch at depth 1000":        `[` + deepCall(sdkMaxDepth-1) + `]`,
	"a batch at depth 1001":        `[` + deepCall(sdkMaxDepth) + `]`,
	"brackets in a string":         `["` + strings.Repeat("[{", sdkMaxDepth) + `"]`,
	"an escaped quote in a string": `["\"` + strings.Repeat("[", sdkMaxDepth) + `"]`,
	"an escaped backslash ends a string": `["\\"` +
		strings.Repeat("[", sdkMaxDepth) + `"]`,
	"closings before the openings": strings.Repeat("]}", 5) +
		strings.Repeat("[", sdkMaxDepth),
	"closings do not go below zero": strings.Repeat("]", 5) +
		strings.Repeat("[", sdkMaxDepth+1),
	"unbalanced": strings.Repeat("[", sdkMaxDepth) + `]` +
		strings.Repeat("[", 2),
	"a backslash at the end":      `["` + strings.Repeat("[", sdkMaxDepth+1) + `\`,
	"not json":                    `x` + strings.Repeat("{", sdkMaxDepth+1),
	"a string never closed":       `"` + strings.Repeat("[", 2*sdkMaxDepth),
	"a body-size run of [":        strings.Repeat("[", maxRequestBodyBytes),
	`a body-size run of {"a":`:    strings.Repeat(`{"a":`, maxRequestBodyBytes/5),
	"a body-size run in a string": `"` + strings.Repeat("[", maxRequestBodyBytes-1),
}

// sdkRefusesForDepth posts body to a stateless streamable HTTP handler
// built as Handler builds /mcp's and reports whether the SDK refused it
// for its nesting: its 400 names the bound sdkMaxDepth claims.
func sdkRefusesForDepth(t *testing.T, body string) bool {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "probe"}, nil)
	h := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true,
			MaxRequestBodyBytes: maxRequestBodyBytes})
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code == http.StatusBadRequest && strings.Contains(
		rec.Body.String(),
		fmt.Sprintf("exceeded maximum nesting depth of %d", sdkMaxDepth))
}

// The account-challenge reader applies the SDK's nesting bound: for every
// body here it refuses exactly the bodies the SDK's handler refuses for
// their nesting, and reads the rest.
func TestTheChallengeReaderAppliesTheSDKsNestingBound(t *testing.T) {
	for name, body := range depthBodies {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, sdkRefusesForDepth(t, body),
				!withinSDKDepth([]byte(body)))
		})
	}
}

// Around the bound, the challenge names the tools the SDK runs, no more
// and no fewer: a call nested deeper than the SDK reads runs nothing and
// is not challenged.
func TestCalledToolsReadsTheBodiesTheSDKReads(t *testing.T) {
	for _, name := range []string{
		"a call at depth 1000", "a call at depth 1001",
		"a batch at depth 1000", "a batch at depth 1001",
	} {
		t.Run(name, func(t *testing.T) {
			body := depthBodies[name]
			assert.ElementsMatch(t, toolsTheSDKRuns(t, body),
				probeTools(calledTools([]byte(body))))
		})
	}
}

// stackBytes reads the memory the runtime holds for goroutine stacks.
func stackBytes() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/stacks:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// maxStackGrowth bounds the stack memory one request may add. Reading a
// body iteratively adds next to nothing; the margin keeps other
// goroutines' stacks from making the test flaky.
const maxStackGrowth = 8 << 20

// An anonymous body nested deeper than the SDK reads goes through the
// account challenge to the SDK, which refuses it, and reading it costs
// the request no more stack than any other.
func TestTheChallengeRefusesABodyNestedDeeperThanTheSDKReads(t *testing.T) {
	store := cartTenant()
	store.Features = EveryFeature
	h := RequireLinkedAccount(Handler(Deps{Site: testSite, Version: "test",
		Log: slog.New(slog.DiscardHandler)}, slog.New(slog.DiscardHandler)))
	// A collection would shrink the stack the request grew before it is
	// read.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	for _, name := range []string{
		"a body-size run of [", `a body-size run of {"a":`,
		"a call at depth 1001", "a batch at depth 1001",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(
				tenant.NewContext(context.Background(), store),
				http.MethodPost, "/mcp", strings.NewReader(depthBodies[name]))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			rec := httptest.NewRecorder()

			// The stack is read on the request's goroutine before it ends:
			// an ended goroutine's grown stack is freed.
			grown := make(chan uint64, 1)
			go func() {
				before := stackBytes()
				h.ServeHTTP(rec, req)
				grown <- max(stackBytes(), before) - before
			}()

			assert.Less(t, <-grown, uint64(maxStackGrowth))
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				rec.Body.String())
			assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
		})
	}
}

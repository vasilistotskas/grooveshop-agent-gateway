package mcptool

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ping = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

func pings(n int) string {
	return "[" + strings.TrimSuffix(strings.Repeat(ping+",", n), ",") + "]"
}

// The scan counts the top level of an array and nothing inside it:
// commas and brackets in strings, escaped quotes and nested values are
// not messages.
func TestBatchMessagesCountsTheTopLevelOnly(t *testing.T) {
	for body, want := range map[string]int{
		`[]`:                             0,
		" \n[ ]":                         0,
		`[1]`:                            1,
		`[ {"a":[1,2,{"b":3}]} , 7 ]`:    2,
		`["a,b]", "c\"],[d", {"e":"]"}]`: 3,
		`[[1,2],[3,4],[5]]`:              3,
		`[1,2,3,`:                        3,
		pings(9):                         9,
	} {
		n, batch := batchMessages([]byte(body))
		assert.True(t, batch, body)
		assert.Equal(t, want, n, body)
	}
	for _, body := range []string{``, ping, ` {"a":[1,2]}`, `"[1,2]"`} {
		_, batch := batchMessages([]byte(body))
		assert.False(t, batch, body)
	}
}

// read posts body with version (none for "") through ReadBody, and says
// whether it was passed on, the bytes passed on and the answer.
func read(
	t *testing.T, version, body string,
) (rec *httptest.ResponseRecorder, passed bool, seen []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(body))
	if version != "" {
		req.Header.Set("Mcp-Protocol-Version", version)
	}
	rec = httptest.NewRecorder()
	got, ok := ReadBody(rec, req, 1<<10)
	return rec, ok, got
}

// From 2025-06-18 a batch is not part of the protocol, so any batch is
// refused, whatever its size, before anything decodes it. A lone
// message is read as it came.
func TestReadBodyRefusesABatchWhereTheProtocolHasNone(t *testing.T) {
	for _, version := range []string{"2025-06-18", "2025-11-25",
		"2026-07-28"} {
		rec, passed, _ := read(t, version, pings(1))
		assert.False(t, passed, version)
		assert.Equal(t, http.StatusBadRequest, rec.Code, version)
		assert.Contains(t, rec.Body.String(),
			"JSON-RPC batching is not supported in 2025-06-18 and later",
			version)

		_, passed, seen := read(t, version, ping)
		assert.True(t, passed, version)
		assert.True(t, bytes.Equal([]byte(ping), seen), "%s: as it came", version)
	}
}

// Where the protocol still has batches (2025-03-26 and 2024-11-05, and a
// request naming no version, read as 2025-03-26), one of up to
// MaxBatchMessages messages is read as it came, and a longer one is
// refused.
func TestReadBodyBoundsABatchWhereTheProtocolHasOne(t *testing.T) {
	for _, version := range []string{"", "2025-03-26", "2024-11-05"} {
		body := pings(MaxBatchMessages)
		_, passed, seen := read(t, version, body)
		assert.True(t, passed, version)
		assert.True(t, bytes.Equal([]byte(body), seen), "%s: as it came", version)

		rec, passed, _ := read(t, version, pings(MaxBatchMessages+1))
		assert.False(t, passed, version)
		assert.Equal(t, http.StatusBadRequest, rec.Code, version)
		assert.Contains(t, rec.Body.String(),
			"a JSON-RPC batch carries at most 8 messages", version)
	}
}

func TestReadBodyBoundsTheBody(t *testing.T) {
	rec, passed, _ := read(t, "", strings.Repeat("x", 1<<10+1))
	assert.False(t, passed)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// A batch the protocol allows still reaches the SDK whole, which answers
// every message in it: the spec's 2025-03-26 "MUST support receiving
// JSON-RPC batches" holds behind ReadBody.
func TestABatchReadBodyPassesIsServedByTheSDK(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "probe"}, nil)
	sdk := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := ReadBody(w, r, 1<<10)
		if !ok {
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		sdk.ServeHTTP(w, r)
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`[`+ping+`,`+
			strings.Replace(ping, `"id":1`, `"id":2`, 1)+`]`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var answers []json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &answers),
		rec.Body.String())
	assert.Len(t, answers, 2)
}

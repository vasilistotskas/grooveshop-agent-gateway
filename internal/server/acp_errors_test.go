package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// acpError checks that rec holds an Error the vendored ACP schema
// accepts, with the status and code wanted, and returns its body.
func acpError(
	t *testing.T, rec *httptest.ResponseRecorder, status int, code string,
) map[string]any {
	t.Helper()
	require.Equal(t, status, rec.Code, "%s", rec.Body)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	f, err := os.Open(filepath.Join("..", "..", "testdata", "schemas", "acp",
		"2026-04-17", "schema.agentic_checkout.json"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	raw, err := jsonschema.UnmarshalJSON(f)
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("acp-checkout.json", raw))
	schema, err := c.Compile("acp-checkout.json#/$defs/Error")
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err, "%s", rec.Body)
	require.NoError(t, schema.Validate(doc), "%s", rec.Body)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, code, body["code"])
	return body
}

func serve(
	h http.Handler, method, host, path string,
) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	req.RemoteAddr = "203.0.113.7:5000"
	req.Header.Set("X-Request-Id", "req-1")
	h.ServeHTTP(rec, req)
	return rec
}

// What answers /acp/* before the ACP handler (the tenant middleware, the
// checkout gate, the mux) answers with the spec's Error, through the
// gateway's own chain, with the status it had. The handler's own answers
// are internal/acp's TestRoutesAnswerSpecErrors.
func TestACPAnswersBeforeTheHandlerAreSpecErrors(t *testing.T) {
	h := gateway(t)
	for _, tc := range []struct {
		name, method, host, path string
		status                   int
		code, typ, allow         string
	}{
		{
			name: "unknown store", method: http.MethodPost,
			host: "unknown.example.test", path: "/acp/checkout_sessions",
			status: http.StatusNotFound, code: "not_found",
			typ: "invalid_request",
		},
		{
			name: "store unavailable", method: http.MethodPost,
			host: "down.example.test", path: "/acp/checkout_sessions",
			status: http.StatusServiceUnavailable, code: "unavailable",
			typ: "service_unavailable",
		},
		{
			name: "checkout off", method: http.MethodPost,
			host: "off.example.test", path: "/acp/checkout_sessions",
			status: http.StatusNotFound, code: "not_found",
			typ: "invalid_request",
		},
		{
			name: "unknown path", method: http.MethodGet,
			host: "unknown.example.test", path: "/acp/nope",
			status: http.StatusNotFound, code: "not_found",
			typ: "invalid_request",
		},
		{
			name: "wrong method", method: http.MethodPut,
			host: "off.example.test", path: "/acp/checkout_sessions",
			status: http.StatusMethodNotAllowed, code: "method_not_allowed",
			typ: "invalid_request", allow: "POST",
		},
		{
			name: "wrong method on a session", method: http.MethodDelete,
			host: "off.example.test", path: "/acp/checkout_sessions/x",
			status: http.StatusMethodNotAllowed, code: "method_not_allowed",
			typ: "invalid_request", allow: "GET, HEAD, POST",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(h, tc.method, tc.host, tc.path)
			body := acpError(t, rec, tc.status, tc.code)
			assert.Equal(t, tc.typ, body["type"])
			assert.Equal(t, tc.allow, rec.Header().Get("Allow"))
		})
	}

	// A store without agent checkout answers a checkout route as an
	// unknown path does: nothing tells the two apart.
	off := serve(h, http.MethodPost, "off.example.test",
		"/acp/checkout_sessions")
	unknown := serve(h, http.MethodGet, "off.example.test", "/acp/nope")
	assert.Equal(t, unknown.Code, off.Code)
	assert.Equal(t, unknown.Header(), off.Header())
	assert.Equal(t, unknown.Body.String(), off.Body.String())
}

// The rate limiter answers /acp/* with the spec's Error and its
// Retry-After, and every other path as before.
func TestACPRateLimitIsASpecError(t *testing.T) {
	h := limitedGateway(t, 1, 1)
	require.Equal(t, http.StatusNotFound, serve(h, http.MethodPost,
		"off.example.test", "/acp/checkout_sessions").Code)
	rec := serve(h, http.MethodPost, "off.example.test",
		"/acp/checkout_sessions")
	body := acpError(t, rec, http.StatusTooManyRequests, "rate_limited")
	assert.Equal(t, "service_unavailable", body["type"])
	assert.Equal(t, "60", rec.Header().Get("Retry-After"))

	// Another store, so another bucket.
	require.Equal(t, http.StatusNotFound, serve(h, http.MethodPost,
		"unknown.example.test", "/mcp").Code)
	other := serve(h, http.MethodPost, "unknown.example.test", "/mcp")
	require.Equal(t, http.StatusTooManyRequests, other.Code)
	assert.Equal(t, "60", other.Header().Get("Retry-After"))
	assert.JSONEq(t, `{"error":"rate limited, retry later"}`,
		other.Body.String())
}

// Other paths keep the answers they had: the mux's text and the
// gateway's own error body.
func TestOtherPathsKeepTheirErrorBodies(t *testing.T) {
	h := gateway(t)
	notFound := serve(h, http.MethodGet, "off.example.test", "/nope")
	assert.Equal(t, http.StatusNotFound, notFound.Code)
	assert.Equal(t, "text/plain; charset=utf-8",
		notFound.Header().Get("Content-Type"))
	assert.Equal(t, "404 page not found\n", notFound.Body.String())

	unknown := serve(h, http.MethodPost, "unknown.example.test", "/mcp")
	assert.Equal(t, http.StatusNotFound, unknown.Code)
	assert.JSONEq(t, `{"error":"unknown store"}`, unknown.Body.String())
}

// A panic under /acp/ is answered with the spec's Error; elsewhere with
// the plain text it had.
func TestAPanicUnderACPIsASpecError(t *testing.T) {
	metrics := obs.NewMetrics()
	mux := http.NewServeMux()
	boom := func(http.ResponseWriter, *http.Request) { panic("boom") }
	mux.HandleFunc("GET /acp/boom", boom)
	mux.HandleFunc("GET /boom", boom)
	h := withGlobalChain(mux, quietLog(), metrics,
		httpmw.NewRateLimiter(6000, 1000, metrics))

	body := acpError(t, serve(h, http.MethodGet, "", "/acp/boom"),
		http.StatusInternalServerError, "internal_error")
	assert.Equal(t, "processing_error", body["type"])

	plain := serve(h, http.MethodGet, "", "/boom")
	assert.Equal(t, http.StatusInternalServerError, plain.Code)
	assert.Equal(t, "text/plain; charset=utf-8",
		plain.Header().Get("Content-Type"))
	assert.Equal(t, "internal server error\n", plain.Body.String())
}

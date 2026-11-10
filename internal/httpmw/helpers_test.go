package httpmw

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name   string
		header string
		token  string
		ok     bool
	}{
		{"canonical", "Bearer abc", "abc", true},
		// RFC 9110: the auth-scheme is case-insensitive.
		{"lowercase scheme", "bearer abc", "abc", true},
		{"surrounding space trimmed", "Bearer  abc ", "abc", true},
		// Present but empty: callers decide (ACP rejects, identity
		// treats it as anonymous).
		{"empty credential", "Bearer ", "", true},
		{"other scheme", "Basic abc", "", false},
		{"no space", "Bearerabc", "", false},
		{"absent", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			token, ok := BearerToken(r)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.token, token)
		})
	}
}

func TestWriteJSONError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSONError(rec, http.StatusNotFound, `unknown "store"`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	// Encoded, not concatenated: quotes in the message stay valid JSON.
	assert.JSONEq(t, `{"error":"unknown \"store\""}`, rec.Body.String())
}

// A handler that returns without writing produces an implicit 200; the
// access log and metrics must record that, not a 0.
func TestStatusWriterDefaultsToOK(t *testing.T) {
	sw := wrap(httptest.NewRecorder())
	assert.Equal(t, http.StatusOK, sw.Status())
	sw.WriteHeader(http.StatusNoContent)
	assert.Equal(t, http.StatusNoContent, sw.Status())
}

// The status recorded is the one the client received: the first final
// one. A later WriteHeader, which net/http ignores, does not replace it,
// and a write or a flush sends the implicit 200.
func TestStatusWriterKeepsTheFirstStatus(t *testing.T) {
	for name, start := range map[string]func(*statusWriter){
		"header": func(sw *statusWriter) { sw.WriteHeader(http.StatusOK) },
		"write":  func(sw *statusWriter) { _, _ = sw.Write([]byte("x")) },
		"flush":  func(sw *statusWriter) { sw.Flush() },
		"after an informational status": func(sw *statusWriter) {
			sw.WriteHeader(http.StatusEarlyHints)
			sw.WriteHeader(http.StatusOK)
		},
	} {
		t.Run(name, func(t *testing.T) {
			sw := wrap(httptest.NewRecorder())
			start(sw)
			sw.WriteHeader(http.StatusInternalServerError)
			assert.Equal(t, http.StatusOK, sw.Status())
		})
	}
}

// http.ResponseController reaches the underlying writer through Unwrap,
// so flushing (SSE) and deadlines keep working behind the chain.
func TestStatusWriterUnwrapsForResponseController(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := wrap(rec)
	assert.Same(t, rec, sw.Unwrap())
	assert.NoError(t, http.NewResponseController(sw).Flush())
	assert.True(t, rec.Flushed)
}

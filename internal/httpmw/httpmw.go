// Package httpmw provides the shared HTTP middleware chain: panic recovery,
// request IDs, structured access logs, RED metrics and per-IP rate limiting.
package httpmw

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// ErrShuttingDown is the cause (context.Cause) of a request context the
// server cancels at the end of its shutdown drain: a streaming handler
// tells it from a client that left, and ends its stream with a final
// event.
var ErrShuttingDown = errors.New("server shutting down")

// WriteJSONError writes the gateway's uniform `{"error": msg}` body. Every
// surface that is not bound to a protocol error shape (ACP has its own)
// answers through it, so agents see one vocabulary.
func WriteJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// BearerToken extracts the credential from an Authorization header. The
// scheme is case-insensitive per RFC 9110; ok reports whether a Bearer
// header was present at all (the token itself may still be empty).
func BearerToken(r *http.Request) (token string, ok bool) {
	scheme, rest, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// statusWriter records the response status for logging and metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Status is the code the client received: a handler that returned without
// writing anything produced net/http's implicit 200, not a 0.
func (w *statusWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Flush forwards flushing so SSE endpoints keep streaming through the
// middleware chain.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func wrap(w http.ResponseWriter) *statusWriter {
	return &statusWriter{ResponseWriter: w}
}

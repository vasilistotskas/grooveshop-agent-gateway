// Package httpmw provides the shared HTTP middleware chain: panic recovery,
// request IDs, structured access logs, RED metrics and per-IP rate limiting.
package httpmw

import (
	"encoding/json"
	"net/http"
	"strings"
)

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

// statusWriter records the response status for logging and metrics: the
// first final status sent. net/http ignores a later WriteHeader (a 500
// after a stream began), and so does the record.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	// 1xx informational responses precede the final status.
	if w.status == 0 && code >= http.StatusOK {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.sent()
	return w.ResponseWriter.Write(b)
}

// sent records net/http's implicit 200, which the first write or flush
// sends.
func (w *statusWriter) sent() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}

// started reports whether a status has been sent.
func (w *statusWriter) started() bool {
	return w.status != 0
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
		w.sent()
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

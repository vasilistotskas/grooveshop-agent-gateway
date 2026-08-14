// Package httpmw provides the shared HTTP middleware chain: panic recovery,
// request IDs, structured access logs, RED metrics and per-IP rate limiting.
package httpmw

import (
	"context"
	"net/http"
	"sync"
)

type ctxKeyRequestID struct{}

// RequestIDFromContext returns the request id set by RequestID.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return id
}

// Extras is a mutable per-request holder that inner handlers fill and the
// access-log middleware reads after the handler returns. It exists because
// tenant resolution happens inside routing, after Logging has already
// captured its request copy — a plain context value set there would be
// invisible to the outer middleware.
type Extras struct {
	mu     sync.Mutex
	tenant string
}

type ctxKeyExtras struct{}

// SetTenant records the resolved tenant schema for the access log.
func SetTenant(ctx context.Context, schema string) {
	if e, ok := ctx.Value(ctxKeyExtras{}).(*Extras); ok {
		e.mu.Lock()
		e.tenant = schema
		e.mu.Unlock()
	}
}

func (e *Extras) getTenant() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tenant
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

// Flush forwards flushing so SSE endpoints keep streaming through the
// middleware chain.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func wrap(w http.ResponseWriter) *statusWriter {
	return &statusWriter{ResponseWriter: w}
}

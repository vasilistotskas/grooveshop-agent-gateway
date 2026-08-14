package tenant

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
)

// Middleware resolves the request's tenant from the real Host header and
// injects it into the context. It deliberately ignores any inbound
// X-Forwarded-Host: Traefik sets Host, and trusting a client-supplied
// forwarded host would let callers pivot between tenant schemas — the exact
// spoofing risk the multi-tenant membership model warns about.
func Middleware(r *Resolver, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			t, err := r.Resolve(req.Context(), req.Host)
			switch {
			case errors.Is(err, ErrUnknownTenant):
				writeJSONError(w, http.StatusNotFound, "unknown store")
				return
			case err != nil:
				log.ErrorContext(req.Context(), "tenant resolution failed",
					slog.String("host", req.Host),
					slog.String("error", err.Error()),
				)
				writeJSONError(w, http.StatusServiceUnavailable,
					"store temporarily unavailable")
				return
			}
			httpmw.SetTenant(req.Context(), t.SchemaName)
			next.ServeHTTP(w, req.WithContext(NewContext(req.Context(), t)))
		})
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}

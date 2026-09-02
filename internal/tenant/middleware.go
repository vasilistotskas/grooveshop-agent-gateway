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
	return middleware(r, log, false)
}

// MiddlewareWithSecrets is Middleware for routes that authenticate
// against a per-tenant credential (/chat, /acp). The Redis cache tier
// stores the PUBLIC config only, so a tenant served from there arrives
// with empty credential fields — indistinguishable, to those handlers,
// from a tenant that never configured the feature. This variant tops
// them up before the handler runs.
func MiddlewareWithSecrets(
	r *Resolver, log *slog.Logger,
) func(http.Handler) http.Handler {
	return middleware(r, log, true)
}

func middleware(
	r *Resolver, log *slog.Logger, needSecrets bool,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			t, err := r.Resolve(req.Context(), req.Host)
			switch {
			case errors.Is(err, ErrUnknownTenant):
				httpmw.WriteJSONError(w, http.StatusNotFound, "unknown store")
				return
			case err != nil:
				log.ErrorContext(req.Context(), "tenant resolution failed",
					slog.String("host", req.Host),
					slog.String("error", err.Error()),
				)
				httpmw.WriteJSONError(w, http.StatusServiceUnavailable,
					"store temporarily unavailable")
				return
			}
			if needSecrets {
				full, secErr := r.EnsureSecrets(req.Context(), t)
				if secErr != nil {
					log.ErrorContext(req.Context(),
						"tenant secret refresh failed",
						slog.String("host", req.Host),
						slog.String("error", secErr.Error()),
					)
					httpmw.WriteJSONError(w, http.StatusServiceUnavailable,
						"store temporarily unavailable")
					return
				}
				t = full
			}
			httpmw.SetTenant(req.Context(), t.SchemaName)
			next.ServeHTTP(w, req.WithContext(NewContext(req.Context(), t)))
		})
	}
}

// RequireAgentCommerce 404s the agent-commerce surfaces (MCP, UCP,
// ACP, chat) when the tenant's effective agent-commerce gate is off.
// Must run INSIDE Middleware — it reads the tenant from the request
// context. 404 (not 403) so a disabled surface is indistinguishable
// from a route that never existed, mirroring Django's feature gates.
func RequireAgentCommerce(next http.Handler) http.Handler {
	return requireFeature(next, func(t *Tenant) bool {
		return t.AgentCommerceOn()
	})
}

// RequireProductFeeds is the catalog-feeds variant (a subordinate
// gate: feeds are off whenever agent commerce is off).
func RequireProductFeeds(next http.Handler) http.Handler {
	return requireFeature(next, func(t *Tenant) bool {
		return t.ProductFeedsOn()
	})
}

func requireFeature(
	next http.Handler, enabled func(*Tenant) bool,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t, ok := FromContext(req.Context())
		if !ok || !enabled(t) {
			httpmw.WriteJSONError(w, http.StatusNotFound, "not found")
			return
		}
		next.ServeHTTP(w, req)
	})
}

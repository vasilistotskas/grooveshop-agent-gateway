package httpmw

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// Logging emits one structured access-log line per request. It gives the
// request the tenant slot that the tenant middleware fills (obs.SetTenant);
// the logger's context handler adds the request id and tenant.
func Logging(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := obs.WithTenantSlot(r.Context())
			sw := wrap(w)
			start := time.Now()
			next.ServeHTTP(sw, r.WithContext(ctx))

			log.LogAttrs(ctx, slog.LevelInfo, "request",
				slog.String("host", r.Host),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.Status()),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
		})
	}
}

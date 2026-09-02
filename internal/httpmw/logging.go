package httpmw

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Logging emits one structured access-log line per request. It installs an
// Extras holder that inner handlers (tenant middleware) enrich via SetTenant.
func Logging(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			extras := &Extras{}
			ctx := context.WithValue(r.Context(), ctxKeyExtras{}, extras)
			sw := wrap(w)
			start := time.Now()
			next.ServeHTTP(sw, r.WithContext(ctx))

			attrs := []slog.Attr{
				slog.String("request_id", RequestIDFromContext(ctx)),
				slog.String("host", r.Host),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.Status()),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			}
			if t := extras.getTenant(); t != "" {
				attrs = append(attrs, slog.String("tenant", t))
			}
			log.LogAttrs(ctx, slog.LevelInfo, "request", attrs...)
		})
	}
}

package httpmw

import (
	"net/http"
	"strconv"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// Metrics records RED metrics per route pattern. r.Pattern is the matched
// ServeMux pattern (Go 1.23+), keeping label cardinality bounded no matter
// what paths clients probe.
func Metrics(m *obs.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := wrap(w)
			start := time.Now()
			next.ServeHTTP(sw, r)

			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			m.HTTPRequests.WithLabelValues(
				route, r.Method, strconv.Itoa(sw.status),
			).Inc()
			m.HTTPDuration.WithLabelValues(route, r.Method).
				Observe(time.Since(start).Seconds())
		})
	}
}

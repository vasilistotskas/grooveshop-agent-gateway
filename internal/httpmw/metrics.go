package httpmw

import (
	"net/http"
	"strconv"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// otherMethod is the method label of every request whose method is not
// one of net/http's constants.
const otherMethod = "other"

// Metrics records RED metrics per route pattern. r.Pattern is the matched
// ServeMux pattern (Go 1.23+) and the method is folded by methodLabel,
// keeping label cardinality bounded no matter what paths and methods
// clients send.
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
			method := methodLabel(r.Method)
			m.HTTPRequests.WithLabelValues(
				route, method, strconv.Itoa(sw.Status()),
			).Inc()
			m.HTTPDuration.WithLabelValues(route, method).
				Observe(time.Since(start).Seconds())
		})
	}
}

// methodLabel keeps the methods RFC 9110 defines and PATCH (RFC 5789),
// net/http's constants; net/http serves any token as a method, and a
// route registered without one reaches its handler with it. Methods are
// case-sensitive (RFC 9110 §9.1), so "get" is not GET.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	}
	return otherMethod
}

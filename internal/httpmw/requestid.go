package httpmw

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// maxRequestIDLen is the 64 characters Django keeps of an inbound
// X-Correlation-ID (core/middleware/correlation_id.py), which is where
// the gateway sends this id. The id is visible ASCII, so a byte is a
// character on both sides and one id means the same to each.
const maxRequestIDLen = 64

// RequestID honors an inbound X-Request-Id (set by upstream proxies) or
// mints one, exposing it on the context and the response. An inbound id
// keeps its visible ASCII only, trimmed and cut to maxRequestIDLen; if
// nothing is left, one is minted.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimSpace(strings.Map(visibleASCII,
				r.Header.Get("X-Request-Id")))
			id = id[:min(len(id), maxRequestIDLen)]
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(w, r.WithContext(
				obs.WithRequestID(r.Context(), id)))
		})
	}
}

// visibleASCII drops controls, DEL and everything outside ASCII (an
// invalid UTF-8 byte arrives as U+FFFD and goes too).
func visibleASCII(r rune) rune {
	if r < 0x20 || r >= 0x7f {
		return -1
	}
	return r
}

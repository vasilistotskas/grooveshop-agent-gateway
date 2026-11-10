package httpmw

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
)

// maxRequestIDRunes is the 64 characters Django keeps of an inbound
// X-Correlation-ID (core/middleware/correlation_id.py), which is where
// the gateway sends this id: one id means the same on both sides.
const maxRequestIDRunes = 64

// RequestID honors an inbound X-Request-Id (set by upstream proxies) or
// mints one, exposing it on the context and the response. An inbound id
// is cleaned as Django cleans a correlation id: ASCII control characters
// dropped, then cut to maxRequestIDRunes.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := text.Runes(strings.Map(dropControl,
				r.Header.Get("X-Request-Id")), maxRequestIDRunes)
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(w, r.WithContext(
				obs.WithRequestID(r.Context(), id)))
		})
	}
}

// dropControl mirrors Django's [\x00-\x1f\x7f]: C0 controls and DEL.
func dropControl(r rune) rune {
	if r < 0x20 || r == 0x7f {
		return -1
	}
	return r
}

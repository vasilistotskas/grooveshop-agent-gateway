package httpmw

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// RequestID honors an inbound X-Request-Id (set by upstream proxies) or
// mints one, exposing it on the context and the response.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set("X-Request-Id", id)
			ctx := context.WithValue(r.Context(), ctxKeyRequestID{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

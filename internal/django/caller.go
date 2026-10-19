package django

import (
	"context"
	"net/http"
)

// caller is who a request to the gateway came from, as Django should
// see it on the calls made for that request. The storefront's SSR proxy
// relays the same headers (server/utils/clientIdentity.ts), so Django
// keys its throttles and Idempotency-Key scopes on the visitor, not on
// the pod that called it.
//
// Nothing here is a decision, and the gateway vouches for nothing: it
// relays what reached it. Django believes the visitor address only
// beside an X-Origin-Verify carrying the secret our Cloudflare zones
// stamp (core/client_ip.py), and otherwise keys on the last
// X-Forwarded-For hop, the peer Traefik appended. The inbound X-Real-Ip
// is never relayed: Traefik sets it to its own peer.
type caller struct {
	realIP       string // CF-Connecting-IP, relayed as X-Real-IP
	forwardedFor string
	originVerify string
	userAgent    string
}

type callerKey struct{}

// RelayCaller has the Django calls made while serving a request carry
// that request's caller.
func RelayCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := caller{
			realIP:       r.Header.Get("CF-Connecting-IP"),
			forwardedFor: r.Header.Get("X-Forwarded-For"),
			originVerify: r.Header.Get("X-Origin-Verify"),
			userAgent:    r.Header.Get("User-Agent"),
		}
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), callerKey{}, c)))
	})
}

// setCaller adds ctx's caller to an outgoing Django request.
func setCaller(ctx context.Context, h http.Header) {
	c, ok := ctx.Value(callerKey{}).(caller)
	if !ok {
		return
	}
	for name, value := range map[string]string{
		"X-Real-IP":       c.realIP,
		"X-Forwarded-For": c.forwardedFor,
		"X-Origin-Verify": c.originVerify,
		"User-Agent":      c.userAgent,
	} {
		if value != "" {
			h.Set(name, value)
		}
	}
}

package httpmw

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// maxLimiterEntries bounds the per-IP bucket map; agent-platform egress IPs
// are few, so the bound only guards against spoofed-source floods.
const maxLimiterEntries = 10_000

// limiterIdleEviction drops buckets untouched for this long.
const limiterIdleEviction = 10 * time.Minute

type limiterEntry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// RateLimiter is a per-IP token bucket. Deliberately in-memory: the gateway
// runs 1–3 pods and Django throttles are the real backstop, so distributed
// precision buys nothing. Note agent platforms share egress IPs — limits
// are generous by design (see plan risk R4).
type RateLimiter struct {
	perMin  int
	burst   int
	metrics *obs.Metrics

	mu      sync.Mutex
	entries map[string]*limiterEntry
}

func NewRateLimiter(perMin, burst int, metrics *obs.Metrics) *RateLimiter {
	return &RateLimiter{
		perMin:  perMin,
		burst:   burst,
		metrics: metrics,
		entries: make(map[string]*limiterEntry),
	}
}

// Middleware rejects over-limit clients with 429 + Retry-After.
func (rl *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.allow(clientIP(r)) {
				if rl.metrics != nil {
					rl.metrics.RateLimited.Inc()
				}
				w.Header().Set("Retry-After", "60")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limited, retry later"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (rl *RateLimiter) allow(ip string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	e, ok := rl.entries[ip]
	if !ok {
		if len(rl.entries) >= maxLimiterEntries {
			rl.evictLocked(now)
		}
		e = &limiterEntry{
			lim: rate.NewLimiter(
				rate.Limit(float64(rl.perMin)/60.0), rl.burst,
			),
		}
		rl.entries[ip] = e
	}
	e.lastSeen = now
	return e.lim.Allow()
}

func (rl *RateLimiter) evictLocked(now time.Time) {
	for k, e := range rl.entries {
		if now.Sub(e.lastSeen) > limiterIdleEviction {
			delete(rl.entries, k)
		}
	}
	// Still full of active entries: drop an arbitrary one rather than grow.
	if len(rl.entries) >= maxLimiterEntries {
		for k := range rl.entries {
			delete(rl.entries, k)
			return
		}
	}
}

// clientIP trusts exactly one proxy hop: Traefik sets X-Real-Ip. Anything
// else falls back to the socket address.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

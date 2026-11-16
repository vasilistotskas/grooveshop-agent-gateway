package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Healthz is process-only liveness: if the process serves, it is alive.
// No dependency checks — a Django or Redis outage must never restart pods.
func Healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

// readyzPingTimeout bounds both of a readiness probe's pings.
const readyzPingTimeout = 2 * time.Second

// Readyz gates readiness on Redis and on the process's startup checks
// (started: nil once they passed), never on Django: checkout sessions
// fail closed without Redis, and a pod whose startup checks have not
// passed (the signing keys) must not take traffic. Django health is
// reported in the body but never flips readiness — with Django down the
// gateway still serves actionable tool errors and stale feeds, which
// beats Traefik 502s.
func Readyz(
	redisPing, djangoPing func(context.Context) error, started func() error,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyzPingTimeout)
		defer cancel()

		body := map[string]string{
			"redis": "ok", "django": "ok", "startup": "ok",
		}
		status := http.StatusOK
		if err := redisPing(ctx); err != nil {
			body["redis"] = err.Error()
			status = http.StatusServiceUnavailable
		}
		if err := started(); err != nil {
			body["startup"] = err.Error()
			status = http.StatusServiceUnavailable
		}
		if err := djangoPing(ctx); err != nil {
			body["django"] = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
}

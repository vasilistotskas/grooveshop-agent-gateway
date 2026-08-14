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

// Readyz gates readiness on Redis only: checkout sessions fail closed
// without it. Django health is reported in the body but never flips
// readiness — with Django down the gateway still serves actionable tool
// errors and stale feeds, which beats Traefik 502s.
func Readyz(redisPing, djangoPing func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		body := map[string]string{"redis": "ok", "django": "ok"}
		status := http.StatusOK
		if err := redisPing(ctx); err != nil {
			body["redis"] = err.Error()
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

// Package server assembles the HTTP mux and middleware chain. Route groups
// added by later milestones (MCP, UCP, ACP, feeds, chat, internal events)
// register here; tenant-scoped groups wrap their handlers with the tenant
// middleware INSIDE routing so route patterns stay visible to metrics.
package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

type Deps struct {
	Cfg      config.Config
	Log      *slog.Logger
	Metrics  *obs.Metrics
	Redis    *redis.Client
	Django   *django.Client
	Resolver *tenant.Resolver
}

func New(d Deps) http.Handler {
	mux := http.NewServeMux()

	redisPing := func(ctx context.Context) error {
		return d.Redis.Ping(ctx).Err()
	}
	mux.Handle("GET /healthz", obs.Healthz())
	mux.Handle("GET /readyz", obs.Readyz(redisPing, d.Django.Ping))
	mux.Handle("GET /metrics", promhttp.HandlerFor(
		d.Metrics.Registry, promhttp.HandlerOpts{},
	))

	// Global chain, outermost first. Metrics sits directly around the mux so
	// r.Pattern (set during routing) is visible when it records.
	limiter := httpmw.NewRateLimiter(
		d.Cfg.RateLimitPerMin, d.Cfg.RateLimitBurst, d.Metrics,
	)
	var h http.Handler = mux
	h = httpmw.Metrics(d.Metrics)(h)
	h = limiter.Middleware()(h)
	h = httpmw.Logging(d.Log)(h)
	h = httpmw.RequestID()(h)
	h = httpmw.Recover(d.Log)(h)
	return h
}

package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics holds the gateway's Prometheus instruments. Tenant is deliberately
// NOT a label anywhere — schema names are unbounded over time and belong in
// logs, not in metric cardinality.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests     *prometheus.CounterVec
	HTTPDuration     *prometheus.HistogramVec
	UpstreamRequests *prometheus.CounterVec
	UpstreamDuration *prometheus.HistogramVec
	TenantCache      *prometheus.CounterVec
	RateLimited      prometheus.Counter
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_http_requests_total",
			Help: "HTTP requests by route pattern, method and status code.",
		}, []string{"route", "method", "code"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_http_request_duration_seconds",
			Help:    "HTTP request duration by route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		UpstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_upstream_requests_total",
			Help: "Django API requests by templated endpoint and status.",
		}, []string{"endpoint", "method", "code"}),
		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_upstream_request_duration_seconds",
			Help:    "Django API request duration by templated endpoint.",
			Buckets: prometheus.DefBuckets,
		}, []string{"endpoint"}),
		TenantCache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_tenant_cache_total",
			Help: "Tenant resolution outcomes: hit, redis_hit, miss, negative, stale.",
		}, []string{"result"}),
		RateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_rate_limited_total",
			Help: "Requests rejected by the gateway rate limiter.",
		}),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.HTTPRequests,
		m.HTTPDuration,
		m.UpstreamRequests,
		m.UpstreamDuration,
		m.TenantCache,
		m.RateLimited,
	)
	return m
}

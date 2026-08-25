// Package server assembles the HTTP mux and middleware chain. Route groups
// added by later milestones (MCP, UCP, ACP, feeds, chat, internal events)
// register here; tenant-scoped groups wrap their handlers with the tenant
// middleware INSIDE routing so route patterns stay visible to metrics.
package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/openai/openai-go/v3/option"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/acp"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/chat"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/feeds"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcpsrv"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

type Deps struct {
	Cfg      config.Config
	Log      *slog.Logger
	Metrics  *obs.Metrics
	Redis    *redis.Client
	Django   *django.Client
	Resolver *tenant.Resolver
	Version  string

	// ChatOpts are extra chat-model client options; the e2e suite
	// injects a fake API base URL here.
	ChatOpts []option.RequestOption

	// Keys serves the per-schema signing keys that sign UCP order
	// webhooks and publish in each tenant's profile.
	Keys *ucp.Keys
	// Dispatcher delivers queued order webhooks (Run started by main).
	Dispatcher *ucp.Dispatcher
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

	// Tenant-scoped surfaces. The tenant middleware wraps handlers inside
	// routing so mux patterns stay visible to the metrics middleware.
	tenantMW := tenant.Middleware(d.Resolver, d.Log)
	// /chat and /acp authenticate against a per-tenant credential, and
	// those are kept out of the shared Redis cache tier — so they need
	// the variant that tops them up.
	tenantSecretsMW := tenant.MiddlewareWithSecrets(d.Resolver, d.Log)

	checkoutStore := checkout.NewStore(d.Redis)
	checkoutFlow := checkout.NewFlow(d.Django, checkoutStore, d.Log)
	ucpBuilder := ucp.NewBuilder(
		d.Django, d.Cfg.MediaURLTemplate, d.Cfg.AssetsHost,
	)

	// The handler builds one server per tenant (they differ only in the
	// store name advertised at initialize) and caches them by schema.
	mcpDeps := mcpsrv.Deps{
		Django:           d.Django,
		Checkout:         checkoutStore,
		Flow:             checkoutFlow,
		UCP:              ucpBuilder,
		MediaURLTemplate: d.Cfg.MediaURLTemplate,
		AssetsHost:       d.Cfg.AssetsHost,
		// Strict by default: only an explicitly non-production ENV
		// relaxes webhook-endpoint validation, so an unset or
		// unrecognised value keeps the public-https rule rather than
		// silently opening the gateway up as a request origin.
		AllowLocalWebhooks: d.Cfg.Env == "development" || d.Cfg.Env == "test",
		Log:                d.Log,
		Version:            d.Version,
	}
	// Identity runs inside the tenant middleware (the upstream token
	// probe needs the tenant). Auth is OPTIONAL — anonymous shopping
	// stays open; a present-but-invalid bearer gets the RFC 9728
	// challenge so MCP clients can (re-)run their OAuth flow.
	identityMW := identity.Middleware(
		identity.NewVerifier(d.Django, d.Log), d.Log)
	mux.Handle("/mcp",
		tenantMW(tenant.RequireAgentCommerce(
			identityMW(mcpsrv.Handler(mcpDeps, d.Log)))))

	mux.Handle("GET /.well-known/ucp",
		tenantMW(tenant.RequireAgentCommerce(ucp.ProfileHandler(d.Keys))))

	// Cluster-internal: Django's order-event push. Not tenant-scoped —
	// the event body carries the schema.
	mux.Handle("POST /internal/events/order-status", internalOrderEvents(
		d.Cfg.InternalSecret, checkoutFlow, d.Dispatcher, d.Log))

	// ACP is always mounted; access is gated per tenant at request time
	// by the tenant's own acpBearerToken from tenant/resolve. A tenant
	// without one has ACP disabled and every bearer gets 401.
	acpMW := func(h http.Handler) http.Handler {
		return tenantSecretsMW(tenant.RequireAgentCommerce(h))
	}
	acp.NewHandler(d.Django, checkoutStore, checkoutFlow, d.Redis,
		d.Log).Register(mux, acpMW)

	feedSvc := feeds.NewService(d.Django, d.Redis, d.Log,
		d.Cfg.FeedImageURLTemplate, d.Cfg.AssetsHost,
		d.Cfg.FeedFreshTTL, d.Cfg.FeedStaleTTL)
	feedMW := func(h http.Handler) http.Handler {
		return tenantMW(tenant.RequireProductFeeds(h))
	}
	mux.Handle("GET /feeds/google.xml", feedMW(feedSvc.Handler(feeds.KindGoogle)))
	mux.Handle("GET /feeds/meta.xml", feedMW(feedSvc.Handler(feeds.KindMeta)))
	mux.Handle("GET /feeds/tiktok.xml", feedMW(feedSvc.Handler(feeds.KindTikTok)))
	mux.Handle("GET /feeds/acp.json", feedMW(feedSvc.Handler(feeds.KindACP)))

	// Chat calls the same toolset in-process. The title is only ever
	// read from an MCP `initialize` response, which this path does not
	// serve — the shopper sees the store name through the system prompt
	// instead — so one shared instance is correct here.
	chatSvc := chat.New(d.Cfg,
		mcpsrv.NewServer(mcpDeps, "Storefront"),
		chat.NewStore(d.Redis, d.Cfg.ConversationTTL, d.Cfg.ChatMaxTurns),
		d.Log,
		d.ChatOpts...,
	)
	// The chat surface pays per token — it gets its own, stricter per-IP
	// limiter on top of the global one. Whether chat is on is a
	// per-tenant question (chatApiKey on tenant/resolve), so the route
	// is always mounted and the handler rejects keyless tenants.
	chatLimiter := httpmw.NewRateLimiter(
		d.Cfg.ChatRatePerMin, d.Cfg.ChatRateBurst, d.Metrics,
	)
	mux.Handle("POST /chat",
		tenantSecretsMW(tenant.RequireAgentCommerce(
			chatLimiter.Middleware()(chatSvc.Handler()))))

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

// Package server assembles the HTTP mux and middleware chain. Every route
// group (MCP, UCP, ACP, feeds, chat, internal events) registers here;
// tenant-scoped groups wrap their handlers with the tenant middleware
// INSIDE routing so route patterns stay visible to metrics.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3/option"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/acp"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/adminmcp"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/chat"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/feeds"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcpsrv"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
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
	// Profiles resolves calling platforms' UCP profiles for capability
	// negotiation.
	Profiles *ucp.ProfileResolver
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

	site := storefront.NewSite(d.Cfg.StorefrontDefaultLocale)
	checkoutStore := checkout.NewStore(d.Redis, d.Django.Timeout())
	checkoutFlow := checkout.NewFlow(d.Django, checkoutStore, site, d.Log)
	ucpBuilder := ucp.NewBuilder(
		d.Django, d.Cfg.MediaURLTemplate, d.Cfg.AssetsHost,
		d.Cfg.PaymentHandlerEnv, site,
	)

	// The handler builds one server per tenant (they differ only in the
	// store name advertised at initialize) and caches them by schema.
	mcpDeps := mcpsrv.Deps{
		Django:             d.Django,
		Checkout:           checkoutStore,
		Flow:               checkoutFlow,
		UCP:                ucpBuilder,
		MediaURLTemplate:   d.Cfg.MediaURLTemplate,
		AssetsHost:         d.Cfg.AssetsHost,
		AllowLocalWebhooks: d.Cfg.AllowLocalWebhooks,
		Profiles:           d.Profiles,
		Site:               site,
		Log:                d.Log,
		Metrics:            d.Metrics,
		// A store's tool list follows its features and identity, which
		// the gateway caches per TTL tier (worst case: CLAUDE.md,
		// internal/mcptool).
		ListTTL: d.Cfg.TenantCacheTTL,
		Version: d.Version,
	}
	// Identity runs inside the tenant middleware (the upstream token
	// probe needs the tenant). Auth is OPTIONAL — anonymous shopping
	// stays open; a present-but-invalid bearer gets the RFC 9728
	// challenge so MCP clients can (re-)run their OAuth flow.
	identityMW := identity.Middleware(
		identity.NewShopperVerifier(d.Django, d.Log), d.Log)
	mux.Handle("/mcp",
		tenantMW(tenant.RequireAgentCommerce(
			identityMW(mcpsrv.RequireLinkedAccount(
				mcpsrv.Handler(mcpDeps))))))
	// The store-admin MCP server: the store's own team, by role, read
	// only. A separate server with its own tools and authorization server
	// (Django's staff issuer on the platform's connect host); every call
	// needs a staff bearer bound to this store (internal/adminmcp).
	mux.Handle(adminmcp.Path,
		tenantMW(adminmcp.RequireStaff(
			adminmcp.NewVerifier(d.Django, d.Log), d.Log)(
			adminmcp.Handler(adminmcp.Deps{
				Django:  d.Django,
				Log:     d.Log,
				Metrics: d.Metrics,
				ListTTL: d.Cfg.TenantCacheTTL,
				Version: d.Version,
			}))))
	mux.Handle("GET /.well-known/oauth-protected-resource"+adminmcp.Path,
		tenantMW(adminmcp.ResourceMetadataHandler(d.Cfg.StaffConnectHost)))
	// Public documents describing the MCP endpoint, readable cross-origin
	// (OPTIONS answers the CORS preflight). They share its gate: a store
	// without an MCP endpoint has nothing to describe.
	describeMCP := func(path string, h http.Handler) {
		gated := tenantMW(tenant.RequireAgentCommerce(h))
		mux.Handle("GET "+path, gated)
		mux.Handle("OPTIONS "+path, gated)
	}
	// The Server Card extension's reserved location: the streamable-HTTP
	// URL + /server-card. Both discovery documents point here.
	describeMCP("/mcp/server-card", mcpsrv.ServerCardHandler(d.Version, site))
	// The ARD manifest, which ARD consumers MUST fetch, and the AI
	// Catalog, which MCP Server Card discovery goes through.
	describeMCP("/.well-known/ard.json", mcpsrv.ARDHandler())
	describeMCP("/.well-known/ai-catalog.json", mcpsrv.AICatalogHandler())

	// Host-scoped so it cannot shadow a tenant path: Go's ServeMux
	// prefers a pattern carrying a host over a path-only one. The
	// documents are static and tenant-independent, so this route takes
	// no tenant middleware and resolves no tenant.
	mux.Handle("GET "+ucp.HandlerDocsHost+"/", ucp.HandlerDocsHandler())

	mux.Handle("GET /.well-known/ucp",
		tenantMW(tenant.RequireCheckout(
			ucp.ProfileHandler(d.Keys, d.Cfg.PaymentHandlerEnv))))

	// Cluster-internal: Django's order-event push. Not tenant-scoped —
	// the event body carries the schema.
	mux.Handle("POST /internal/events/order-status", internalOrderEvents(
		d.Cfg.InternalSecret, orderEventDeps{
			Store: checkoutStore, Flow: checkoutFlow, Resolver: d.Resolver,
			Django: d.Django, Dispatcher: d.Dispatcher, Site: site,
			Log: d.Log,
		}))

	// ACP is always mounted; access is gated per tenant at request time
	// by the tenant's own acpBearerToken from tenant/resolve. A tenant
	// without one has ACP disabled and every bearer gets 401.
	acpMW := func(h http.Handler) http.Handler {
		return tenantSecretsMW(tenant.RequireCheckout(h))
	}
	acp.NewHandler(d.Django, checkoutStore, checkoutFlow, site, d.Log).
		Register(mux, acpMW)

	feedSvc := feeds.NewService(d.Django, d.Redis, d.Log, d.Metrics,
		d.Cfg.FeedImageURLTemplate, d.Cfg.AssetsHost,
		d.Cfg.FeedFreshTTL, d.Cfg.FeedStaleTTL, d.Version, site)
	feedMW := func(h http.Handler) http.Handler {
		return tenantMW(tenant.RequireProductFeeds(h))
	}
	mux.Handle("GET /feeds/google.xml", feedMW(feedSvc.Handler(feeds.KindGoogle)))
	mux.Handle("GET /feeds/meta.xml", feedMW(feedSvc.Handler(feeds.KindMeta)))
	mux.Handle("GET /feeds/tiktok.xml", feedMW(feedSvc.Handler(feeds.KindTikTok)))
	mux.Handle("GET /feeds/acp.json", feedMW(feedSvc.Handler(feeds.KindACP)))

	// Cluster-internal: lets Django drop a tenant's cached feeds when the
	// catalogue changes, instead of the merchant waiting out
	// FEED_FRESH_TTL. Registered here rather than beside the other
	// internal route because it needs feedSvc, which is built above.
	mux.Handle("POST /internal/feeds/invalidate", internalFeedInvalidate(
		d.Cfg.InternalSecret, feedSvc, d.Log))

	// Chat calls the same toolset in-process. The title is only ever
	// read from an MCP `initialize` response, which this path does not
	// serve — the shopper sees the store name through the system prompt
	// instead — so one shared instance is correct here. It carries every
	// tool; the bridge offers each store only its own (mcpsrv.Offered).
	chatSvc := chat.New(d.Cfg,
		mcpsrv.NewChatServer(mcpDeps),
		chat.NewStore(d.Redis, d.Cfg.ConversationTTL, d.Cfg.ChatMaxTurns),
		chat.NewQuota(d.Redis,
			d.Cfg.ChatStoreTurnsPerHour, d.Cfg.ChatVisitorTurnsPerDay),
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

	limiter := httpmw.NewRateLimiter(
		d.Cfg.RateLimitPerMin, d.Cfg.RateLimitBurst, d.Metrics,
	)
	return withGlobalChain(mux, d.Log, d.Metrics, limiter)
}

// withGlobalChain wraps the mux in the middleware every route shares,
// built from the mux outwards.
func withGlobalChain(
	mux http.Handler, log *slog.Logger, metrics *obs.Metrics,
	limiter *httpmw.RateLimiter,
) http.Handler {
	// Recover is innermost, so a handler panic reaches Metrics and
	// Logging as a 500 and its line carries the request id and tenant.
	// It hands the mux the request it got, and Metrics, directly around
	// it, sees r.Pattern, which the mux sets during routing.
	h := httpmw.Recover(log)(mux)
	h = httpmw.Metrics(metrics)(h)
	// Outside Metrics: it hands the mux a new request, whose r.Pattern
	// Metrics would not see.
	h = django.RelayCaller(h)
	h = exceptInternal(limiter.Middleware(), h)
	h = httpmw.Logging(log)(h)
	return httpmw.RequestID()(h)
}

// exceptInternal applies mw to every route but /internal/*. Those are
// Django's own workers pushing over the cluster Service (no ingress
// routes them) and authenticated by the shared secret; a per-IP bucket
// would throttle a status burst from one Celery pod, and Django treats
// a 4xx as final and drops the event.
func exceptInternal(
	mw func(http.Handler) http.Handler, next http.Handler,
) http.Handler {
	limited := mw(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			next.ServeHTTP(w, r)
			return
		}
		limited.ServeHTTP(w, r)
	})
}

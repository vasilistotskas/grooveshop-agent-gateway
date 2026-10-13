// Package mcpsrv exposes the commerce toolset over MCP (stateless
// streamable HTTP). One Server per tenant carries every tool — they
// differ only in the store name they advertise at initialize; the
// tenant itself arrives per request via the tenant middleware's context
// value, which the SDK propagates into tool handlers.
package mcpsrv

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// Deps are the shared dependencies for all tool handlers.
type Deps struct {
	Django           *django.Client
	Checkout         *checkout.Store
	Flow             *checkout.Flow
	UCP              *ucp.Builder
	MediaURLTemplate string
	AssetsHost       string
	// AllowLocalWebhooks relaxes webhook-endpoint validation for
	// development and the e2e suite, which register httptest servers on
	// 127.0.0.1. Production keeps the strict public-https rule.
	AllowLocalWebhooks bool
	// Profiles resolves the calling platform's UCP profile for
	// capability negotiation on every canonical tool.
	Profiles *ucp.ProfileResolver
	Log      *slog.Logger
	Version  string
}

// NewServer builds the MCP server with the commerce toolset a store with
// features is served: a tool whose feature is off is neither listed nor
// callable. id is what it calls itself in `initialize` — per tenant, and
// identical to the tenant's Server Card (IdentityFor).
func NewServer(d Deps, id Identity, features django.StoreFeatures) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    id.Name,
		Title:   id.Title,
		Version: d.Version,
	}, &mcp.ServerOptions{
		// Tools only. The SDK default also advertises logging (deprecated
		// as of 2026-07-28) and tools.listChanged — a notification a
		// stateless server has no session to send.
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{},
		},
	})
	for _, t := range toolset(&handlers{deps: d}) {
		if t.offeredTo(features) {
			t.register(srv)
		}
	}
	return srv
}

// EveryFeature builds the chat bridge's server, which one in-process
// client shares across stores: it carries every tool, and the bridge
// offers each store only the ones it is served (Offered).
var EveryFeature = django.StoreFeatures{
	ProductReviews: true,
	Favourites:     true,
	Loyalty:        true,
	Promotions:     true,
}

// tool is one entry of the toolset: its definition, and the typed
// handler bound to it (which a plain slice of *mcp.Tool cannot carry,
// as every handler has its own argument and result types).
type tool struct {
	def      *mcp.Tool
	register func(*mcp.Server)
	// offered, when set, is the store feature the tool serves: a store
	// with it off is not served the tool.
	offered func(django.StoreFeatures) bool
}

// onlyWith offers t only to a store with the feature on.
func onlyWith(on func(django.StoreFeatures) bool, t tool) tool {
	t.offered = on
	return t
}

func (t tool) offeredTo(f django.StoreFeatures) bool {
	return t.offered == nil || t.offered(f)
}

// The store features a tool can serve, for onlyWith.
func reviewsOn(f django.StoreFeatures) bool    { return f.ProductReviews }
func loyaltyOn(f django.StoreFeatures) bool    { return f.Loyalty }
func favouritesOn(f django.StoreFeatures) bool { return f.Favourites }

func toolOf[In, Out any](def *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) tool {
	return tool{def: def, register: func(srv *mcp.Server) {
		mcp.AddTool(srv, def, h)
	}}
}

// definitions is the toolset without handlers, read once: each tool's
// name and the feature it serves.
var definitions = sync.OnceValue(func() []tool { return toolset(&handlers{}) })

// toolNamesFor lists the tools NewServer registers for a store with
// features, in order — the `capabilities` of its discovery entry. Read off
// the same toolset, so a new tool is advertised the moment it is served.
func toolNamesFor(features django.StoreFeatures) []string {
	var names []string
	for _, t := range definitions() {
		if t.offeredTo(features) {
			names = append(names, t.def.Name)
		}
	}
	return names
}

// Offered reports whether a store with features is served the named tool.
// The chat bridge asks it, because it shares one server across stores.
func Offered(name string, features django.StoreFeatures) bool {
	for _, t := range definitions() {
		if t.def.Name == name {
			return t.offeredTo(features)
		}
	}
	return false
}

// representativeQueries are requests a shopper's agent makes that this
// toolset serves: catalog search, delivery and pickup options, cart and
// checkout, order tracking. ARD registries build their semantic index
// from them (ARD v0.91 §4.2 wants 2–5); store is the merchant's title.
func representativeQueries(store string) []string {
	return []string{
		"What does " + store + " sell, and how much does it cost?",
		"Which delivery and parcel-locker pickup options does " + store +
			" offer near my postal code?",
		"Add this product to my " + store + " cart and give me the " +
			"checkout link",
		"Where is my " + store + " order, and what is its tracking link?",
	}
}

// toolset is every tool the server carries, with its handler on h.
func toolset(h *handlers) []tool {
	return []tool{
		toolOf(&mcp.Tool{
			Name: "search_products",
			Description: "Search the store's product catalog by free text " +
				"with optional category and price filters. Greek and English " +
				"queries both work (Greeklish is expanded server-side). " +
				"Returns product ids usable with get_product; prices are " +
				"VAT-inclusive.",
		}, h.searchProducts),

		toolOf(&mcp.Tool{
			Name: "get_product",
			Description: "Fetch one product by id: localized name and " +
				"description, VAT-inclusive price, live stock, rating " +
				"(1–10, when the store has reviews), page URL, image and " +
				"purchasable variants.",
		}, h.getProduct),

		toolOf(&mcp.Tool{
			Name: "list_categories",
			Description: "List the store's product category tree (ids and " +
				"localized names). Category ids can be passed to " +
				"search_products to narrow a search.",
		}, h.listCategories),

		toolOf(&mcp.Tool{
			Name: "get_trending_searches",
			Description: "Popular search queries in this store over the last " +
				"24 hours — useful to discover what shoppers are looking at. " +
				"Feed a query into search_products to see the products.",
		}, h.getTrendingSearches),

		onlyWith(reviewsOn, toolOf(&mcp.Tool{
			Name: "get_product_reviews",
			Description: "Customer reviews for a product: overall rating and " +
				"each review's rating (both 1–10) and recent " +
				"verified-purchase comments.",
		}, h.getProductReviews)),

		toolOf(&mcp.Tool{
			Name: "get_shipping_options",
			Description: "Available delivery methods for a destination " +
				"country with prices and free-shipping thresholds. Kinds: " +
				"home_delivery and pickup_point (lockers/shops).",
		}, h.getShippingOptions),

		toolOf(&mcp.Tool{
			Name: "find_pickup_points",
			Description: "Find parcel pickup points near a Greek postal " +
				"code: ACS Smartpoint lockers/shops and BOX NOW lockers. " +
				"Pass city (and optionally street) for more accurate BOX " +
				"NOW results.",
		}, h.findPickupPoints),

		toolOf(&mcp.Tool{
			Name: "get_payment_methods",
			Description: "Payment methods this store accepts (e.g. card via " +
				"Viva/Stripe, cash on delivery), with any extra cost and " +
				"free thresholds. Optionally filter by the shipping " +
				"provider/kind you plan to use.",
		}, h.getPaymentMethods),

		toolOf(&mcp.Tool{
			Name: "create_cart",
			Description: "Create an empty shopping cart. Persist the returned " +
				"cartId — every cart and checkout tool needs it. add_to_cart " +
				"also creates a cart implicitly when called without one.",
		}, h.createCart),

		toolOf(&mcp.Tool{
			Name: "get_cart",
			Description: "Fetch a cart's current lines and totals by cartId, " +
				"including any applied coupon, promotion discount and " +
				"promotional free shipping.",
		}, h.getCart),

		toolOf(&mcp.Tool{
			Name: "add_to_cart",
			Description: "Add a product to the cart (increments the line if " +
				"already present). Omit cartId on the first call to create " +
				"the cart implicitly; persist the returned cartId.",
		}, h.addToCart),

		toolOf(&mcp.Tool{
			Name:        "update_cart_item",
			Description: "Change a cart line's quantity.",
		}, h.updateCartItem),

		toolOf(&mcp.Tool{
			Name:        "remove_cart_item",
			Description: "Remove a line from the cart.",
		}, h.removeCartItem),

		toolOf(&mcp.Tool{
			Name: "get_checkout_link",
			Description: "Get the URL where the shopper completes checkout in " +
				"their browser — reviewing the cart, choosing delivery and " +
				"paying (card via the store's payment provider, or cash on " +
				"delivery). Payment always happens on the store's own pages, " +
				"never in chat.",
		}, h.getCheckoutLink),

		toolOf(&mcp.Tool{
			Name: "track_order",
			Description: "Track an existing order by its UUID (found in the " +
				"confirmation email): fulfilment status, payment status and " +
				"the carrier tracking link once shipped.",
		}, h.trackOrder),

		toolOf(&mcp.Tool{
			Name: "subscribe_product_alert",
			Description: "Subscribe an email address to a product alert: " +
				"restock (back in stock) or price_drop (price falls to a " +
				"target). Useful when a product is out of stock or too " +
				"expensive right now.",
		}, h.subscribeProductAlert),

		// Account-scoped tools: need a linked shopper account (OAuth
		// authorization-code + PKCE against the store's identity provider;
		// discovery via /.well-known/oauth-protected-resource/mcp). The
		// bearer token travels on the MCP HTTP request's Authorization
		// header and is forwarded to Django, which enforces scopes.
		toolOf(&mcp.Tool{
			Name: "my_orders",
			Description: "The linked shopper's recent orders (requires a " +
				"connected account with the orders:read scope). Returns " +
				"status, payment state and totals; use track_order with an " +
				"orderUuid for carrier tracking.",
		}, h.myOrders),

		onlyWith(loyaltyOn, toolOf(&mcp.Tool{
			Name: "my_loyalty_points",
			Description: "The linked shopper's loyalty summary — spendable " +
				"points, level and tier (requires a connected account with " +
				"the loyalty:read scope).",
		}, h.myLoyaltyPoints)),

		onlyWith(favouritesOn, toolOf(&mcp.Tool{
			Name: "my_favourites",
			Description: "The linked shopper's favourite products — the " +
				"basis for personalised recommendations (requires a " +
				"connected account with the favourites:read scope).",
		}, h.myFavourites)),

		// UCP checkout capability (dev.ucp.shopping.checkout), MCP transport
		// binding. Tool names, argument names and argument shapes come from
		// the OpenRPC document the profile advertises, so a platform that
		// generates calls from it reaches these unmodified: `meta` carries
		// protocol metadata, `id` names the target session, and `checkout`
		// carries the domain object. Structured output is the UCP checkout
		// session.
		toolOf(&mcp.Tool{
			Name:         "create_checkout",
			OutputSchema: checkoutResultSchema,
			Description: "UCP: start a checkout session. Send " +
				"checkout.line_items (or cart_id for a cart built with the " +
				"cart tools). Collect buyer, fulfillment and payment with " +
				"update_checkout until status is ready_for_complete. " +
				"Requires meta.ucp-agent.profile.",
		}, h.createCheckout),

		toolOf(&mcp.Tool{
			Name:         "get_checkout",
			OutputSchema: checkoutResultSchema,
			Description: "UCP: read the current state of a checkout " +
				"session, including its totals, messages and the payment " +
				"instruments available for it. Requires " +
				"meta.ucp-agent.profile.",
		}, h.getCheckout),

		toolOf(&mcp.Tool{
			Name:         "update_checkout",
			OutputSchema: checkoutResultSchema,
			Description: "UCP: add or change buyer details, delivery " +
				"(address or ACS/BOX NOW pickup point), payment and " +
				"discount codes on a checkout session. Requires " +
				"meta.ucp-agent.profile.",
		}, h.updateCheckout),

		toolOf(&mcp.Tool{
			Name:         "complete_checkout",
			OutputSchema: checkoutResultSchema,
			Description: "UCP: place the order. Send " +
				"checkout.payment.instruments naming one instrument the " +
				"checkout advertised. Offline methods (cash on delivery) " +
				"complete immediately; card payment via Viva returns " +
				"continue_url where the buyer authorizes on the store's " +
				"hosted page. Requires meta.ucp-agent.profile and " +
				"meta.idempotency-key.",
		}, h.completeCheckout),

		// UCP order capability (dev.ucp.shopping.order), MCP transport
		// binding. Its conformance section requires exactly this tool.
		toolOf(&mcp.Tool{
			Name:         "get_order",
			OutputSchema: orderResultSchema,
			Description: "UCP: the current state of an order placed through " +
				"this surface — line items with per-line fulfilment status, " +
				"totals and the permalink. Use track_order for shipment " +
				"tracking or for orders placed on the web. Requires " +
				"meta.ucp-agent.profile.",
		}, h.getOrder),

		toolOf(&mcp.Tool{
			Name:         "cancel_checkout",
			OutputSchema: checkoutResultSchema,
			Description: "UCP: abandon a checkout session the buyer is no " +
				"longer pursuing. Already-canceled sessions succeed " +
				"unchanged. Requires meta.ucp-agent.profile and " +
				"meta.idempotency-key.",
		}, h.cancelCheckout),
	}
}

// Handler wraps the server in the stateless streamable HTTP transport.
func Handler(d Deps, log *slog.Logger) http.Handler {
	cache := &serverCache{deps: d, servers: map[string]*mcp.Server{}}
	return mcp.NewStreamableHTTPHandler(
		cache.forRequest,
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			PropagateRequestCancellation: true,
			// Tool arguments are a few KB at most; the SDK default of
			// 4 MiB is a lot of buffering for an anonymous endpoint.
			MaxRequestBodyBytes: 256 << 10,
			Logger:              log,
		},
	)
}

// serverCache hands out one MCP server per tenant identity.
//
// A store is served the tools of the features it has on, under its own
// identity, so building a server costs one pass over the toolset, done
// once per store and feature set rather than per request. The tenant
// middleware wraps this handler from outside, so the request context
// already carries the resolved tenant.
type serverCache struct {
	mu      sync.Mutex
	deps    Deps
	servers map[string]*mcp.Server
}

// maxCachedServers bounds the map the way the UCP key cache is bounded:
// schemas are operator-created and few, but nothing should grow without
// a ceiling. Past it the map is dropped and rebuilt on demand.
const maxCachedServers = 512

// forRequest returns nil — which the SDK answers with 400 — for a request
// without a tenant; the tenant middleware in front makes that unreachable.
func (c *serverCache) forRequest(r *http.Request) *mcp.Server {
	t, ok := tenant.FromContext(r.Context())
	if !ok {
		return nil
	}
	return c.get(t.SchemaName, IdentityFor(t), t.Features)
}

// get keys by schema, identity AND features, so a store rename reaches
// initialize, and a feature switched on or off reaches tools/list, without
// a restart; the superseded entry ages out with the map bound.
func (c *serverCache) get(
	schema string, id Identity, features django.StoreFeatures,
) *mcp.Server {
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%+v", schema, id.Name, id.Title, features)
	c.mu.Lock()
	defer c.mu.Unlock()
	if srv, hit := c.servers[key]; hit {
		return srv
	}
	if len(c.servers) >= maxCachedServers {
		c.servers = map[string]*mcp.Server{}
	}
	srv := NewServer(c.deps, id, features)
	c.servers[key] = srv
	return srv
}

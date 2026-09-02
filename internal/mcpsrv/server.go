// Package mcpsrv exposes the commerce toolset over MCP (stateless
// streamable HTTP). One Server per tenant carries every tool — they
// differ only in the store name they advertise at initialize; the
// tenant itself arrives per request via the tenant middleware's context
// value, which the SDK propagates into tool handlers.
package mcpsrv

import (
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
	Log                *slog.Logger
	Version            string
}

// NewServer builds the MCP server with the full commerce toolset.
//
// title is what the server calls itself in the `initialize` response.
// It is per-tenant: on a white-label storefront an agent must be told
// it is talking to the MERCHANT, not to the platform behind them. The
// neighbouring surfaces already do this — /.well-known/ucp is per
// tenant and the feeds resolve the merchant name — so a hardcoded
// platform title here was the odd one out.
//
// Name stays constant: it is the protocol identifier, not a display
// name.
// defaultTitle is advertised when a tenant carries no store name at all.
const defaultTitle = "Storefront"

func NewServer(d Deps, title string) *mcp.Server {
	if title == "" {
		title = defaultTitle
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "grooveshop-agent-gateway",
		Title:   title,
		Version: d.Version,
	}, nil)

	h := &handlers{deps: d}

	mcp.AddTool(srv, &mcp.Tool{
		Name: "search_products",
		Description: "Search the store's product catalog by free text " +
			"with optional category and price filters. Greek and English " +
			"queries both work (Greeklish is expanded server-side). " +
			"Returns product ids usable with get_product; prices are " +
			"VAT-inclusive.",
	}, h.searchProducts)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_product",
		Description: "Fetch one product by id: localized name and " +
			"description, VAT-inclusive price, live stock, rating, page " +
			"URL, image and purchasable variants.",
	}, h.getProduct)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_categories",
		Description: "List the store's product category tree (ids and " +
			"localized names). Category ids can be passed to " +
			"search_products to narrow a search.",
	}, h.listCategories)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_trending_searches",
		Description: "Popular search queries in this store over the last " +
			"24 hours — useful to discover what shoppers are looking at. " +
			"Feed a query into search_products to see the products.",
	}, h.getTrendingSearches)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_product_reviews",
		Description: "Customer reviews for a product: overall rating and " +
			"recent verified-purchase comments.",
	}, h.getProductReviews)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_shipping_options",
		Description: "Available delivery methods for a destination " +
			"country with prices and free-shipping thresholds. Kinds: " +
			"home_delivery and pickup_point (lockers/shops).",
	}, h.getShippingOptions)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "find_pickup_points",
		Description: "Find parcel pickup points near a Greek postal " +
			"code: ACS Smartpoint lockers/shops and BOX NOW lockers. " +
			"Pass city (and optionally street) for more accurate BOX " +
			"NOW results.",
	}, h.findPickupPoints)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_payment_methods",
		Description: "Payment methods this store accepts (e.g. card via " +
			"Viva/Stripe, cash on delivery), with any extra cost and " +
			"free thresholds. Optionally filter by the shipping " +
			"provider/kind you plan to use.",
	}, h.getPaymentMethods)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_cart",
		Description: "Create an empty shopping cart. Persist the returned " +
			"cartId — every cart and checkout tool needs it. add_to_cart " +
			"also creates a cart implicitly when called without one.",
	}, h.createCart)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_cart",
		Description: "Fetch a cart's current lines and totals by cartId, " +
			"including any applied coupon, promotion discount and " +
			"promotional free shipping.",
	}, h.getCart)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "add_to_cart",
		Description: "Add a product to the cart (increments the line if " +
			"already present). Omit cartId on the first call to create " +
			"the cart implicitly; persist the returned cartId.",
	}, h.addToCart)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "update_cart_item",
		Description: "Change a cart line's quantity.",
	}, h.updateCartItem)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "remove_cart_item",
		Description: "Remove a line from the cart.",
	}, h.removeCartItem)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_checkout_link",
		Description: "Get the URL where the shopper completes checkout in " +
			"their browser — reviewing the cart, choosing delivery and " +
			"paying (card via the store's payment provider, or cash on " +
			"delivery). Payment always happens on the store's own pages, " +
			"never in chat.",
	}, h.getCheckoutLink)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "track_order",
		Description: "Track an existing order by its UUID (found in the " +
			"confirmation email): fulfilment status, payment status and " +
			"the carrier tracking link once shipped.",
	}, h.trackOrder)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "subscribe_product_alert",
		Description: "Subscribe an email address to a product alert: " +
			"restock (back in stock) or price_drop (price falls to a " +
			"target). Useful when a product is out of stock or too " +
			"expensive right now.",
	}, h.subscribeProductAlert)

	// Account-scoped tools: need a linked shopper account (OAuth
	// authorization-code + PKCE against the store's identity provider;
	// discovery via /.well-known/oauth-protected-resource/mcp). The
	// bearer token travels on the MCP HTTP request's Authorization
	// header and is forwarded to Django, which enforces scopes.
	mcp.AddTool(srv, &mcp.Tool{
		Name: "my_orders",
		Description: "The linked shopper's recent orders (requires a " +
			"connected account with the orders:read scope). Returns " +
			"status, payment state and totals; use track_order with an " +
			"orderUuid for carrier tracking.",
	}, h.myOrders)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "my_loyalty_points",
		Description: "The linked shopper's loyalty summary — spendable " +
			"points, level and tier (requires a connected account with " +
			"the loyalty:read scope).",
	}, h.myLoyaltyPoints)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "my_favourites",
		Description: "The linked shopper's favourite products — the " +
			"basis for personalised recommendations (requires a " +
			"connected account with the favourites:read scope).",
	}, h.myFavourites)

	// UCP checkout capability (dev.ucp.shopping.checkout), MCP transport
	// binding. Tool names, argument names and argument shapes come from
	// the OpenRPC document the profile advertises, so a platform that
	// generates calls from it reaches these unmodified: `meta` carries
	// protocol metadata, `id` names the target session, and `checkout`
	// carries the domain object. Structured output is the UCP checkout
	// session.
	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_checkout",
		Description: "UCP: start a checkout session. Send " +
			"checkout.line_items (or cart_id for a cart built with the " +
			"cart tools). Collect buyer, fulfillment and payment with " +
			"update_checkout until status is ready_for_complete. " +
			"Requires meta.ucp-agent.profile.",
	}, h.createCheckout)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_checkout",
		Description: "UCP: read the current state of a checkout " +
			"session, including its totals, messages and the payment " +
			"instruments available for it. Requires " +
			"meta.ucp-agent.profile.",
	}, h.getCheckout)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "update_checkout",
		Description: "UCP: add or change buyer details, delivery " +
			"(address or ACS/BOX NOW pickup point), payment and " +
			"discount codes on a checkout session. Requires " +
			"meta.ucp-agent.profile.",
	}, h.updateCheckout)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "complete_checkout",
		Description: "UCP: place the order. Send " +
			"checkout.payment.instruments naming one instrument the " +
			"checkout advertised. Offline methods (cash on delivery) " +
			"complete immediately; card payment via Viva returns " +
			"continue_url where the buyer authorizes on the store's " +
			"hosted page. Requires meta.ucp-agent.profile and " +
			"meta.idempotency-key.",
	}, h.completeCheckout)

	// UCP order capability (dev.ucp.shopping.order), MCP transport
	// binding. Its conformance section requires exactly this tool.
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_order",
		Description: "UCP: the current state of an order placed through " +
			"this surface — line items with per-line fulfilment status, " +
			"totals and the permalink. Use track_order for shipment " +
			"tracking or for orders placed on the web. Requires " +
			"meta.ucp-agent.profile.",
	}, h.getOrder)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "cancel_checkout",
		Description: "UCP: abandon a checkout session the buyer is no " +
			"longer pursuing. Already-canceled sessions succeed " +
			"unchanged. Requires meta.ucp-agent.profile and " +
			"meta.idempotency-key.",
	}, h.cancelCheckout)

	return srv
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
			Logger:                       log,
		},
	)
}

// serverCache hands out one MCP server per tenant schema.
//
// The tool set is identical for every tenant — only the advertised
// title differs — so building a server costs one pass over the AddTool
// calls, done once per schema rather than per request. The tenant
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

func (c *serverCache) forRequest(r *http.Request) *mcp.Server {
	t, ok := tenant.FromContext(r.Context())
	if !ok || t == nil {
		return c.get("", defaultTitle)
	}
	title := t.StoreName
	if title == "" {
		title = t.Name
	}
	return c.get(t.SchemaName, title)
}

func (c *serverCache) get(schema, title string) *mcp.Server {
	c.mu.Lock()
	defer c.mu.Unlock()
	if srv, hit := c.servers[schema]; hit {
		return srv
	}
	if len(c.servers) >= maxCachedServers {
		c.servers = map[string]*mcp.Server{}
	}
	srv := NewServer(c.deps, title)
	c.servers[schema] = srv
	return srv
}

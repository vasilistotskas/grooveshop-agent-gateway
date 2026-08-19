// Package mcpsrv exposes the commerce toolset over MCP (stateless
// streamable HTTP). One immutable Server carries every tool; the tenant
// arrives per request via the tenant middleware's context value, which the
// SDK propagates into tool handlers.
package mcpsrv

import (
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
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
	Log              *slog.Logger
	Version          string
}

// NewServer builds the MCP server with the full commerce toolset.
func NewServer(d Deps) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "grooveshop-agent-gateway",
		Title:   "GrooveShop storefront",
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
		Name:        "get_cart",
		Description: "Fetch a cart's current lines and totals by cartId.",
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

	// UCP checkout capability (dev.ucp.shopping.checkout, MCP transport
	// binding). Structured output is the UCP checkout session object.
	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_checkout",
		Description: "UCP: start a checkout session from a cartId or a " +
			"list of products. Collect buyer, fulfillment and payWayId " +
			"(via update_checkout or inline) until status is " +
			"ready_for_complete.",
	}, h.createCheckout)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "update_checkout",
		Description: "UCP: add or change buyer details, delivery " +
			"(address or ACS/BOX NOW pickup point) and payment method " +
			"on a checkout session.",
	}, h.updateCheckout)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "complete_checkout",
		Description: "UCP: place the order. Offline payment methods " +
			"(e.g. cash on delivery) complete immediately; card payment " +
			"via Viva returns continue_url where the buyer authorizes " +
			"payment on the store's hosted page.",
	}, h.completeCheckout)

	return srv
}

// Handler wraps the server in the stateless streamable HTTP transport.
func Handler(srv *mcp.Server, log *slog.Logger) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			PropagateRequestCancellation: true,
			Logger:                       log,
		},
	)
}

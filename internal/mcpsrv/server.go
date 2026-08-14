// Package mcpsrv exposes the commerce toolset over MCP (stateless
// streamable HTTP). One immutable Server carries every tool; the tenant
// arrives per request via the tenant middleware's context value, which the
// SDK propagates into tool handlers.
package mcpsrv

import (
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// Deps are the shared dependencies for all tool handlers.
type Deps struct {
	Django           *django.Client
	MediaURLTemplate string
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

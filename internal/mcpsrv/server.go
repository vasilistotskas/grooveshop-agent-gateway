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

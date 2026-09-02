// Package storefront builds links into a tenant's Nuxt storefront and
// the gateway's own tenant-domain endpoints. The paths are a cross-repo
// contract with grooveshop-storefront-ui-node-nuxt (app/pages and
// server/routes), so they live in exactly one place: a route rename
// there is a one-line change here, not a hunt through every renderer.
package storefront

import (
	"net/url"
	"strconv"
)

// Origin is the https origin of a tenant domain.
func Origin(domain string) string {
	return "https://" + domain
}

// Home is the storefront landing page.
func Home(domain string) string {
	return Origin(domain) + "/"
}

// Product is the product detail page (app/pages/products/[id]/[slug]).
func Product(domain string, id int64, slug string) string {
	return Origin(domain) + "/products/" + strconv.FormatInt(id, 10) +
		"/" + url.PathEscape(slug)
}

// Cart is the shopper's cart page (app/pages/cart).
func Cart(domain string) string {
	return Origin(domain) + "/cart"
}

// CartClaim hands a gateway-built cart to the browser session
// (server/routes/cart/claim.get.ts).
func CartClaim(domain, cartID string) string {
	return Origin(domain) + "/cart/claim?uuid=" + url.QueryEscape(cartID)
}

// OrderSuccess is the post-checkout confirmation page
// (app/pages/checkout/success/[uuid]).
func OrderSuccess(domain, orderUUID string) string {
	return Origin(domain) + "/checkout/success/" + url.PathEscape(orderUUID)
}

// Terms is the terms-of-use legal page (app/pages/terms-of-use.vue).
func Terms(domain string) string {
	return Origin(domain) + "/terms-of-use"
}

// Privacy is the privacy-policy legal page
// (app/pages/privacy-policy.vue).
func Privacy(domain string) string {
	return Origin(domain) + "/privacy-policy"
}

// MCP is this gateway's MCP endpoint, path-routed on the tenant domain.
func MCP(domain string) string {
	return Origin(domain) + "/mcp"
}

// OAuthResourceMetadata is the RFC 9728 protected-resource document the
// storefront publishes for the MCP endpoint
// (server/routes/.well-known/oauth-protected-resource/mcp.get.ts).
func OAuthResourceMetadata(domain string) string {
	return Origin(domain) + "/.well-known/oauth-protected-resource/mcp"
}

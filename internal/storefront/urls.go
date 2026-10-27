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

// Site is the storefront's routing, the same for every tenant. Its
// unprefixed pages render defaultLocale, the storefront build's default
// language (DEFAULT_LOCALE in its i18n/locales.ts) — on every tenant,
// whatever the tenant's own default; any other language lives under
// /<code> (@nuxtjs/i18n's prefix_except_default).
type Site struct {
	defaultLocale string
}

// NewSite describes a storefront whose build default is defaultLocale
// (config.StorefrontDefaultLocale).
func NewSite(defaultLocale string) Site {
	return Site{defaultLocale: defaultLocale}
}

// Links are the links into domain's storefront pages in lang: the
// turn's language in the chat, the store's default elsewhere.
func (s Site) Links(domain, lang string) Links {
	l := Links{origin: Origin(domain)}
	if lang != s.defaultLocale {
		l.locale = lang
	}
	return l
}

// Links builds the page links of one storefront in one language.
type Links struct {
	origin string
	// locale is the language a link must name: empty for the build
	// default, which the unprefixed pages render.
	locale string
}

// page is path under the language's prefix.
func (l Links) page(path string) string {
	if l.locale == "" {
		return l.origin + path
	}
	if path == "/" {
		return l.origin + "/" + l.locale
	}
	return l.origin + "/" + l.locale + path
}

// Home is the storefront landing page.
func (l Links) Home() string {
	return l.page("/")
}

// Product is the product detail page (app/pages/products/[id]/[slug]).
func (l Links) Product(id int64, slug string) string {
	return l.page("/products/" + strconv.FormatInt(id, 10) + "/" +
		url.PathEscape(slug))
}

// Cart is the shopper's cart page (app/pages/cart).
func (l Links) Cart() string {
	return l.page("/cart")
}

// Checkout is the checkout page (app/pages/checkout).
func (l Links) Checkout() string {
	return l.page("/checkout")
}

// CartClaim hands a gateway-built cart to the browser session
// (server/routes/cart/claim.get.ts). A server route takes no language
// prefix: it names the language as `locale` so it can redirect to the
// cart page in it, and names none for the build default.
func (l Links) CartClaim(cartID string) string {
	q := url.Values{"uuid": {cartID}}
	if l.locale != "" {
		q.Set("locale", l.locale)
	}
	return l.origin + "/cart/claim?" + q.Encode()
}

// OrderSuccess is the post-checkout confirmation page
// (app/pages/checkout/success/[uuid]).
func (l Links) OrderSuccess(orderUUID string) string {
	return l.page("/checkout/success/" + url.PathEscape(orderUUID))
}

// Terms is the terms-of-use legal page (app/pages/terms-of-use.vue).
func (l Links) Terms() string {
	return l.page("/terms-of-use")
}

// Privacy is the privacy-policy legal page
// (app/pages/privacy-policy.vue).
func (l Links) Privacy() string {
	return l.page("/privacy-policy")
}

// ReturnPolicy is the return-policy page (app/pages/return-policy.vue),
// which renders the store's published `return-policy` content page.
func (l Links) ReturnPolicy() string {
	return l.page("/return-policy")
}

// MCP is this gateway's MCP endpoint, path-routed on the tenant domain.
func MCP(domain string) string {
	return Origin(domain) + "/mcp"
}

// MCPServerCard is this gateway's MCP Server Card, at the location the
// Server Card extension reserves: the streamable-HTTP URL + /server-card.
func MCPServerCard(domain string) string {
	return MCP(domain) + "/server-card"
}

// UCPProfile is this gateway's UCP business profile on the tenant
// domain — the identity order webhooks name in UCP-Agent.
func UCPProfile(domain string) string {
	return Origin(domain) + "/.well-known/ucp"
}

// OAuthResourceMetadata is the RFC 9728 protected-resource document the
// storefront publishes for the MCP endpoint
// (server/routes/.well-known/oauth-protected-resource/mcp.get.ts).
func OAuthResourceMetadata(domain string) string {
	return Origin(domain) + "/.well-known/oauth-protected-resource/mcp"
}

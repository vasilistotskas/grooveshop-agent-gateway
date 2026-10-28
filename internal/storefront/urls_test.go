package storefront

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const d = "shop.example.test"

// site is a storefront built with el as its default language.
var site = NewSite("el")

// Every path here is a route the Nuxt storefront actually serves; the
// old per-renderer literals had drifted (a /terms-and-conditions link
// pointed at a page that does not exist). In the build's default
// language the pages are unprefixed.
func TestLinksInTheBuildDefaultAreUnprefixed(t *testing.T) {
	l := site.Links(d, "el")
	assert.Equal(t, "https://shop.example.test/", l.Home())
	assert.Equal(t, "https://shop.example.test/products/42/blue-mug",
		l.Product(42, "blue-mug"))
	assert.Equal(t, "https://shop.example.test/cart", l.Cart())
	assert.Equal(t, "https://shop.example.test/checkout", l.Checkout())
	assert.Equal(t, "https://shop.example.test/cart/claim?uuid=c-1",
		l.CartClaim("c-1"))
	assert.Equal(t, "https://shop.example.test/checkout/success/o-1",
		l.OrderSuccess("o-1"))
	assert.Equal(t, "https://shop.example.test/terms-of-use", l.Terms())
	assert.Equal(t, "https://shop.example.test/privacy-policy", l.Privacy())
	assert.Equal(t, "https://shop.example.test/return-policy",
		l.ReturnPolicy())
}

// An English chat turn linked the Greek pages: any other language lives
// under /<code>, and the cart claim, a server route that takes no
// prefix, names it for its redirect instead.
func TestLinksInAnotherLanguageArePrefixed(t *testing.T) {
	l := site.Links(d, "en")
	assert.Equal(t, "https://shop.example.test/en", l.Home())
	assert.Equal(t, "https://shop.example.test/en/products/42/blue-mug",
		l.Product(42, "blue-mug"))
	assert.Equal(t, "https://shop.example.test/en/cart", l.Cart())
	assert.Equal(t, "https://shop.example.test/en/checkout", l.Checkout())
	assert.Equal(t,
		"https://shop.example.test/cart/claim?locale=en&uuid=c-1",
		l.CartClaim("c-1"))
	assert.Equal(t, "https://shop.example.test/en/checkout/success/o-1",
		l.OrderSuccess("o-1"))
	assert.Equal(t, "https://shop.example.test/en/terms-of-use", l.Terms())
	assert.Equal(t, "https://shop.example.test/en/privacy-policy",
		l.Privacy())
	assert.Equal(t, "https://shop.example.test/en/return-policy",
		l.ReturnPolicy())
}

// The unprefixed pages render the BUILD default on every tenant, so a
// store whose own default differs links its default prefixed.
func TestTheBuildDefaultDecidesThePrefixNotTheStore(t *testing.T) {
	assert.Equal(t, "https://shop.example.test/cart",
		NewSite("en").Links(d, "en").Cart())
	assert.Equal(t, "https://shop.example.test/el/cart",
		NewSite("en").Links(d, "el").Cart())
}

func TestGatewayEndpoints(t *testing.T) {
	assert.Equal(t, "https://shop.example.test", Origin(d))
	assert.Equal(t, "https://shop.example.test/mcp", MCP(d))
	assert.Equal(t, "https://shop.example.test/mcp/server-card",
		MCPServerCard(d))
	assert.Equal(t,
		"https://shop.example.test/.well-known/oauth-protected-resource/mcp",
		OAuthResourceMetadata(d))
}

// Ids and slugs come from upstream or from the agent; they are escaped
// so a stray character cannot break out of the path or query.
func TestURLsEscapeSegments(t *testing.T) {
	l := site.Links(d, "el")
	assert.Equal(t, "https://shop.example.test/products/1/%CE%B8%CE%AE%CE%BA%CE%B7",
		l.Product(1, "θήκη"))
	assert.Equal(t, "https://shop.example.test/cart/claim?uuid=a%26b%3Dc",
		l.CartClaim("a&b=c"))
	assert.Equal(t, "https://shop.example.test/checkout/success/x%2Fy",
		l.OrderSuccess("x/y"))
}

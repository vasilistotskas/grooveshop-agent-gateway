package storefront

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every path here is a route the Nuxt storefront actually serves; the
// old per-renderer literals had drifted (a /terms-and-conditions link
// pointed at a page that does not exist).
func TestURLs(t *testing.T) {
	const d = "shop.example.test"
	assert.Equal(t, "https://shop.example.test", Origin(d))
	assert.Equal(t, "https://shop.example.test/", Home(d))
	assert.Equal(t, "https://shop.example.test/products/42/blue-mug",
		Product(d, 42, "blue-mug"))
	assert.Equal(t, "https://shop.example.test/cart", Cart(d))
	assert.Equal(t, "https://shop.example.test/cart/claim?uuid=c-1",
		CartClaim(d, "c-1"))
	assert.Equal(t, "https://shop.example.test/checkout/success/o-1",
		OrderSuccess(d, "o-1"))
	assert.Equal(t, "https://shop.example.test/terms-of-use", Terms(d))
	assert.Equal(t, "https://shop.example.test/privacy-policy", Privacy(d))
	assert.Equal(t, "https://shop.example.test/mcp", MCP(d))
	assert.Equal(t,
		"https://shop.example.test/.well-known/oauth-protected-resource/mcp",
		OAuthResourceMetadata(d))
}

// Ids and slugs come from upstream or from the agent; they are escaped
// so a stray character cannot break out of the path or query.
func TestURLsEscapeSegments(t *testing.T) {
	const d = "shop.example.test"
	assert.Equal(t, "https://shop.example.test/products/1/%CE%B8%CE%AE%CE%BA%CE%B7",
		Product(d, 1, "θήκη"))
	assert.Equal(t, "https://shop.example.test/cart/claim?uuid=a%26b%3Dc",
		CartClaim(d, "a&b=c"))
	assert.Equal(t, "https://shop.example.test/checkout/success/x%2Fy",
		OrderSuccess(d, "x/y"))
}

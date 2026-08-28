package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImageURL(t *testing.T) {
	tpl := "https://{assets_host}/media_stream-image/{path}" +
		"/800/800/contain/entropy/transparent/5/80.webp"

	got := ImageURL(tpl, "assets.platform.test", "demostore",
		"media/uploads/products/foo.jpg")
	assert.Equal(t,
		"https://assets.platform.test/media_stream-image/"+
			"media/uploads/products/foo.jpg"+
			"/800/800/contain/entropy/transparent/5/80.webp",
		got)

	assert.Empty(t, ImageURL(tpl, "assets.platform.test", "demostore", ""))
}

func TestImageURLSchemaPlaceholder(t *testing.T) {
	// The multi-tenant media-stream URL shape embeds the schema; flipping
	// the template env at cutover must be the only change required.
	tpl := "https://{assets_host}/media_stream-image/media/{schema}" +
		"/uploads/{path}/800/800/contain/entropy/transparent/5/80.webp"

	got := ImageURL(tpl, "assets.platform.test", "demostore", "products/foo.jpg")
	assert.Contains(t, got, "/media/demostore/uploads/products/foo.jpg/")
}

// An unresolved media origin must drop the image rather than publish an
// unreachable URL: ad platforms reject items with unfetchable images and
// nothing here ever fetches what it emits, so a broken host is silent.
func TestImageURLWithoutHostIsEmpty(t *testing.T) {
	tpl := "https://{assets_host}/media_stream-image/{path}/800/800.webp"
	assert.Empty(t, ImageURL(tpl, "", "demostore", "media/uploads/foo.jpg"))
}

func TestHostPrefersTenantAssetsDomain(t *testing.T) {
	// A tenant that opted into white-label asset URLs serves from its own
	// host; everyone else shares the platform origin. Deriving
	// assets.<storefront-domain> instead produced a hostname that standard
	// onboarding never creates.
	assert.Equal(t, "assets.tenant-b.com",
		Host("assets.tenant-b.com", "assets.platform.test"))
	assert.Equal(t, "assets.platform.test",
		Host("", "assets.platform.test"))
	assert.Empty(t, Host("", ""))
}

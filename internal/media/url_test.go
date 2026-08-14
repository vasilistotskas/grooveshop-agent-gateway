package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImageURL(t *testing.T) {
	tpl := "https://assets.{domain}/media_stream-image/{path}" +
		"/800/800/contain/entropy/transparent/5/80.webp"

	got := ImageURL(tpl, "shop.example.test", "webside",
		"media/uploads/products/foo.jpg")
	assert.Equal(t,
		"https://assets.shop.example.test/media_stream-image/"+
			"media/uploads/products/foo.jpg"+
			"/800/800/contain/entropy/transparent/5/80.webp",
		got)

	assert.Empty(t, ImageURL(tpl, "shop.example.test", "webside", ""))
}

func TestImageURLSchemaPlaceholder(t *testing.T) {
	// The multi-tenant media-stream URL shape embeds the schema; flipping
	// the template env at cutover must be the only change required.
	tpl := "https://assets.{domain}/media_stream-image/media/{schema}" +
		"/uploads/{path}/800/800/contain/entropy/transparent/5/80.webp"

	got := ImageURL(tpl, "shop.example.test", "webside", "products/foo.jpg")
	assert.Contains(t, got, "/media/webside/uploads/products/foo.jpg/")
}

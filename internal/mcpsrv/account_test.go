package mcpsrv

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func TestMyFavouritesPricing(t *testing.T) {
	var auth string
	h := fixtureHandlers(t, "agent_favourites.json", func(r *http.Request) {
		auth = r.Header.Get("Authorization")
	})
	ctx := identity.NewContext(
		tenant.NewContext(context.Background(), cartTenant()),
		&identity.Linked{Bearer: "linked-token"})

	res, out, err := h.myFavourites(ctx, nil, struct{}{})
	require.NoError(t, err)
	assert.Equal(t, "Bearer linked-token", auth)
	require.Len(t, out.Favourites, 3)

	announced := out.Favourites[0]
	assert.Equal(t, "428.72", announced.EffectivePrice)
	assert.Equal(t, "476.35", announced.PriorPrice)
	assert.Equal(t, "10", announced.AnnouncedDiscountPercent)
	assert.Equal(t, "EUR", announced.Currency)

	plain := out.Favourites[1]
	assert.Equal(t, "17.91", plain.EffectivePrice)
	assert.Empty(t, plain.PriorPrice)
	assert.Empty(t, plain.AnnouncedDiscountPercent)

	assert.Empty(t, out.Favourites[2].EffectivePrice)

	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "- Pressure Cooker Final (product 6125): "+
		"428.72 EUR (VAT included), 10% off 476.35 EUR, its lowest "+
		"price in the 30 days before the reduction, in stock")
	assert.Contains(t, text, "- Multivitamin Supplements (product 2): "+
		"17.91 EUR (VAT included), OUT OF STOCK")
	assert.Contains(t, text, "- Ceramic Teapot (product 37): "+
		"price being updated, see the product page, in stock")
}

package mcpsrv

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// A chat turn's add sends the shopper's first touch the storefront
// relayed, so the cart records the visit's real source; an /mcp agent's
// add, or a turn that relayed none, sends no attribution.
func TestAddToCartSendsTheShopperAttribution(t *testing.T) {
	status := http.StatusCreated
	h, _, adds := addDjango(t, &status)
	ctx := tenant.NewContext(context.Background(), cartTenant())
	in := AddToCartIn{CartID: fixtureCartID, ProductID: 1}

	_, _, err := h.addToCart(WithShopperAttribution(ctx,
		&django.Attribution{UtmSource: "instagram", UtmMedium: "paid"}),
		nil, in)
	require.NoError(t, err)
	_, _, err = h.addToCart(ctx, nil, in)
	require.NoError(t, err)

	require.Len(t, *adds, 2)
	assert.Equal(t,
		map[string]any{"utmSource": "instagram", "utmMedium": "paid"},
		(*adds)[0]["attribution"])
	assert.NotContains(t, (*adds)[1], "attribution")
}

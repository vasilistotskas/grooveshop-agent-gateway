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
// relayed, so the cart records the visit's real source; a turn that
// relayed none sends none, never an agent's protocol.
func TestChatAddToCartSendsTheShopperAttribution(t *testing.T) {
	status := http.StatusCreated
	h, _, adds := addDjango(t, &status)
	h.surface = chatSurface
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

// An /mcp agent's add books the cart as an AI agent's, whatever the
// context carries.
func TestMCPAddToCartBooksTheCartAsAnAgents(t *testing.T) {
	status := http.StatusCreated
	h, _, adds := addDjango(t, &status)
	h.surface = mcpSurface
	ctx := tenant.NewContext(context.Background(), cartTenant())
	in := AddToCartIn{CartID: fixtureCartID, ProductID: 1}

	_, _, err := h.addToCart(ctx, nil, in)
	require.NoError(t, err)
	_, _, err = h.addToCart(WithShopperAttribution(ctx,
		&django.Attribution{UtmSource: "instagram"}), nil, in)
	require.NoError(t, err)

	require.Len(t, *adds, 2)
	for _, add := range *adds {
		assert.Equal(t, map[string]any{"agentProtocol": "mcp"},
			add["attribution"])
	}
}

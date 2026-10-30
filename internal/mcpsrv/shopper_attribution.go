package mcpsrv

import (
	"context"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

type shopperAttributionKey struct{}

// WithShopperAttribution has add_to_cart called with ctx send a: the
// chat shopper's first touch, which the storefront relays with the turn.
// Django records it as the source of the cart the add first fills.
func WithShopperAttribution(
	ctx context.Context, a *django.Attribution,
) context.Context {
	return context.WithValue(ctx, shopperAttributionKey{}, a)
}

// cartAttribution is what an add_to_cart sends as the cart's source. On
// /mcp an agent fills the cart, booked as one; in the chat it is the
// shopper's first touch WithShopperAttribution set, or none, and never
// an agent's: the shopper fills their own cart through the assistant.
func (h *handlers) cartAttribution(ctx context.Context) *django.Attribution {
	if h.surface == mcpSurface {
		return &django.Attribution{AgentProtocol: django.AgentProtocolMCP}
	}
	a, _ := ctx.Value(shopperAttributionKey{}).(*django.Attribution)
	return a
}

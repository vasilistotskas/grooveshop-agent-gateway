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

// shopperAttribution is the attribution WithShopperAttribution set, or
// nil (an /mcp agent, or a chat turn that relayed none).
func shopperAttribution(ctx context.Context) *django.Attribution {
	a, _ := ctx.Value(shopperAttributionKey{}).(*django.Attribution)
	return a
}

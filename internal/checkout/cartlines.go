package checkout

import (
	"context"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Line is one requested product line.
type Line struct {
	ProductID int64
	Quantity  int
}

// SyncCartLines makes the Django cart hold exactly lines: quantities
// update, new products append, omitted lines are removed. Both protocols
// treat a submitted line list as the full desired state, so a surface
// that applied only additions would complete an order the buyer no
// longer wants. A product named on several lines is one cart line of
// their sum: Django keeps one line per product, and its add stacks, so
// two writes for one product would end at the sum on a new cart and at
// the last quantity on a cart that already holds it. protocol
// (ProtocolUCP | ProtocolACP) is the surface filling the cart, recorded
// as its source.
func SyncCartLines(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	protocol, cartID string, lines []Line,
) error {
	cart, err := dj.GetCart(ctx, t.Domain, t.DefaultLocale, cartID)
	if err != nil {
		return err
	}
	existing := make(map[int64]django.CartItem, len(cart.Items))
	for _, it := range cart.Items {
		existing[it.Product.ID] = it
	}
	lines = mergeLines(lines)
	wanted := make(map[int64]bool, len(lines))
	for _, l := range lines {
		wanted[l.ProductID] = true
		if line, ok := existing[l.ProductID]; ok {
			if line.Quantity != l.Quantity {
				if _, err := dj.UpdateCartItem(ctx, t.Domain,
					t.DefaultLocale, cartID, line.ID, l.Quantity); err != nil {
					return err
				}
			}
			continue
		}
		if _, err := dj.AddCartItem(ctx, t.Domain, t.DefaultLocale,
			cartID, l.ProductID, l.Quantity,
			agentAttribution(protocol)); err != nil {
			return err
		}
	}
	for productID, line := range existing {
		if !wanted[productID] {
			if err := dj.RemoveCartItem(ctx, t.Domain, t.DefaultLocale,
				cartID, line.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// mergeLines folds the lines of one product into one, summing their
// quantities, in the order each product first appears.
func mergeLines(lines []Line) []Line {
	at := make(map[int64]int, len(lines))
	merged := make([]Line, 0, len(lines))
	for _, l := range lines {
		if i, ok := at[l.ProductID]; ok {
			merged[i].Quantity += l.Quantity
			continue
		}
		at[l.ProductID] = len(merged)
		merged = append(merged, l)
	}
	return merged
}

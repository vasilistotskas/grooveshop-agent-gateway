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
// longer wants.
func SyncCartLines(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	cartID string, lines []Line,
) error {
	cart, err := dj.GetCart(ctx, t.Domain, t.DefaultLocale, cartID)
	if err != nil {
		return err
	}
	existing := make(map[int64]django.CartItem, len(cart.Items))
	for _, it := range cart.Items {
		existing[it.Product.ID] = it
	}
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
			cartID, l.ProductID, l.Quantity); err != nil {
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

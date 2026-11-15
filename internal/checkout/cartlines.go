package checkout

import (
	"context"
	"errors"
	"fmt"

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
// their sum (MergeLines). protocol (ProtocolUCP | ProtocolACP) is the
// surface filling the cart, recorded as its source.
func SyncCartLines(
	ctx context.Context, dj *django.Client, t *tenant.Tenant,
	protocol, cartID string, lines []Line,
) error {
	lines, err := MergeLines(lines)
	if err != nil {
		return err
	}
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

// MaxQuantity is the largest quantity a line may ask for, UCP's own
// bound (line_item.json): a sum of two lines within it is exact.
const MaxQuantity = 1<<53 - 1

// ErrInvalidQuantity refuses a line whose quantity is not a count the
// store could take: below 1, or above MaxQuantity once a product's lines
// are summed.
var ErrInvalidQuantity = errors.New(
	"a line item's quantity must be at least 1 and at most 9007199254740991")

// MergeLines folds the lines of one product into one, summing their
// quantities, in the order each product first appears. Django keeps one
// line per product, and its add stacks: two writes for one product would
// end at the sum on a new cart and at the last quantity on a cart that
// already holds it.
//
// A quantity below 1, or a product's sum above MaxQuantity, is
// ErrInvalidQuantity, never folded into another line: the store would
// refuse such a line, and a merge must not turn it into a quantity
// nobody asked for. Each surface calls it where it reads its lines, so a
// refused request writes nothing to the cart.
func MergeLines(lines []Line) ([]Line, error) {
	at := make(map[int64]int, len(lines))
	merged := make([]Line, 0, len(lines))
	for _, l := range lines {
		if l.Quantity < 1 {
			return nil, fmt.Errorf("product %d asks for %d: %w",
				l.ProductID, l.Quantity, ErrInvalidQuantity)
		}
		i, ok := at[l.ProductID]
		if !ok {
			at[l.ProductID] = len(merged)
			merged = append(merged, Line{ProductID: l.ProductID})
			i = len(merged) - 1
		}
		if l.Quantity > MaxQuantity-merged[i].Quantity {
			return nil, fmt.Errorf("product %d's lines add up to more "+
				"than the maximum: %w", l.ProductID, ErrInvalidQuantity)
		}
		merged[i].Quantity += l.Quantity
	}
	return merged, nil
}

package mcpsrv

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// CartItemOut is one cart line in tool output.
type CartItemOut struct {
	ItemID     int64  `json:"itemId" jsonschema:"line id for update_cart_item/remove_cart_item"`
	ProductID  int64  `json:"productId"`
	Name       string `json:"name"`
	Quantity   int    `json:"quantity"`
	UnitPrice  string `json:"unitPrice" jsonschema:"VAT-inclusive"`
	TotalPrice string `json:"totalPrice"`
}

type CartOut struct {
	CartID     string        `json:"cartId" jsonschema:"persist this and pass it to every cart and checkout tool"`
	Items      []CartItemOut `json:"items"`
	TotalItems int           `json:"totalItems"`
	Total      string        `json:"total" jsonschema:"VAT-inclusive items total, before promotionDiscount"`
	Currency   string        `json:"currency"`
	// Discount fields are present only when non-zero.
	PromotionDiscount  string             `json:"promotionDiscount,omitempty" jsonschema:"promotion/coupon discount subtracted from total at checkout"`
	TotalDiscountValue string             `json:"totalDiscountValue,omitempty" jsonschema:"product markdown savings already included in item prices"`
	AppliedCouponCodes []string           `json:"appliedCouponCodes,omitempty" jsonschema:"coupon codes that currently apply to this cart"`
	RefusedCoupons     []RefusedCouponOut `json:"refusedCoupons,omitempty" jsonschema:"coupon codes attached to the cart that do not apply now, with the store's reason"`
	FreeShipping       bool               `json:"freeShipping,omitempty" jsonschema:"a promotion grants free delivery on this cart"`
	TotalWeightGrams   int                `json:"totalWeightGrams" jsonschema:"cart weight; pass it to get_shipping_options so options over a weight cap are left out"`
}

// RefusedCouponOut is an attached coupon the store refuses now — its item
// left the cart, a better automatic offer won, its gift ran out.
type RefusedCouponOut struct {
	Code   string `json:"code"`
	Reason string `json:"reason" jsonschema:"the store's discount_code_* reason"`
}

func (h *handlers) cartOut(t *tenant.Tenant, c *django.Cart) CartOut {
	out := CartOut{
		CartID:             c.UUID,
		TotalItems:         c.TotalItems,
		Total:              num(c.TotalPrice),
		Currency:           t.DefaultCurrency,
		Items:              make([]CartItemOut, 0, len(c.Items)),
		PromotionDiscount:  posNum(c.PromotionDiscount),
		TotalDiscountValue: posNum(c.TotalDiscountValue),
		FreeShipping:       c.PromotionFreeShipping,
		TotalWeightGrams:   c.TotalWeightGrams,
	}
	for _, coupon := range c.AppliedCoupons {
		if coupon.Rejection != "" {
			out.RefusedCoupons = append(out.RefusedCoupons, RefusedCouponOut{
				Code: coupon.Code, Reason: coupon.Rejection,
			})
			continue
		}
		out.AppliedCouponCodes = append(out.AppliedCouponCodes, coupon.Code)
	}
	for _, it := range c.Items {
		tr := django.Localized(it.Product.Translations, t.DefaultLocale)
		out.Items = append(out.Items, CartItemOut{
			ItemID:     it.ID,
			ProductID:  it.Product.ID,
			Name:       tr.Name,
			Quantity:   it.Quantity,
			UnitPrice:  num(it.FinalPrice),
			TotalPrice: num(it.TotalPrice),
		})
	}
	return out
}

func (h *handlers) cartSummary(out CartOut) *mcp.CallToolResult {
	var note strings.Builder
	if out.PromotionDiscount != "" {
		fmt.Fprintf(&note, " A promotion discount of %s %s",
			out.PromotionDiscount, out.Currency)
		if len(out.AppliedCouponCodes) > 0 {
			fmt.Fprintf(&note, " (coupon %s)",
				strings.Join(out.AppliedCouponCodes, ", "))
		}
		note.WriteString(" applies at checkout.")
	}
	if out.FreeShipping {
		note.WriteString(" Shipping is free on this cart.")
	}
	for _, refused := range out.RefusedCoupons {
		fmt.Fprintf(&note, " Coupon %s is attached but does not apply (%s).",
			refused.Code, refused.Reason)
	}
	return textResult(
		"Cart %s holds %d items totalling %s %s (VAT included).%s Persist "+
			"cartId for later calls; hand the shopper get_checkout_link "+
			"when they're ready to pay.",
		out.CartID, out.TotalItems, out.Total, out.Currency, note.String(),
	)
}

type CreateCartOut struct {
	CartID string `json:"cartId" jsonschema:"persist this and pass it to every cart and checkout tool"`
}

func (h *handlers) createCart(
	ctx context.Context, _ *mcp.CallToolRequest, _ struct{},
) (*mcp.CallToolResult, CreateCartOut, error) {
	var out CreateCartOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, "")
	if err != nil {
		return nil, out, upstreamErr(err, "the cart service is unavailable")
	}
	out.CartID = c.UUID
	// The token resolves to the cart the signed-in shopper already has:
	// nothing is created, and saying so keeps the model from presenting
	// their existing cart as a new, empty one.
	if django.HasCartToken(ctx) {
		return textResult(
			"No cart was created: the shopper is signed in and already "+
				"has cart %s, which the cart tools act on. Fetch it with "+
				"get_cart to see what is in it.", out.CartID,
		), out, nil
	}
	cartChanged(ctx)
	return textResult(
		"Created cart %s. Persist this cartId — it identifies the "+
			"shopper's cart in every subsequent call.", out.CartID,
	), out, nil
}

// cartId is optional in the schemas below: a signed-in shopper's chat
// turn reaches their cart through the cart token instead, and the
// handlers require it of everyone else.
type GetCartIn struct {
	CartID string `json:"cartId,omitempty" jsonschema:"cart id from create_cart or add_to_cart"`
}

func (h *handlers) getCart(
	ctx context.Context, _ *mcp.CallToolRequest, in GetCartIn,
) (*mcp.CallToolResult, CartOut, error) {
	var out CartOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	if in.CartID == "" && !django.HasCartToken(ctx) {
		return nil, out, errors.New(
			"cartId is required; create one with create_cart")
	}
	c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, in.CartID)
	if err != nil {
		return nil, out, upstreamErr(err, "that cart no longer exists; "+
			"create a new one with create_cart")
	}
	out = h.cartOut(t, c)
	return h.cartSummary(out), out, nil
}

type AddToCartIn struct {
	CartID    string `json:"cartId,omitempty" jsonschema:"omit on the first add to create a cart implicitly"`
	ProductID int64  `json:"productId"`
	Quantity  int    `json:"quantity,omitempty" jsonschema:"default 1, max 999"`
}

func (h *handlers) addToCart(
	ctx context.Context, _ *mcp.CallToolRequest, in AddToCartIn,
) (*mcp.CallToolResult, CartOut, error) {
	var out CartOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	if in.ProductID <= 0 {
		return nil, out, errors.New("productId is required")
	}
	qty := in.Quantity
	if qty <= 0 {
		qty = 1
	}

	// A signed-in shopper's cart is reached through their cart token,
	// whatever id is named: there is no guest cart to create first.
	cartID := in.CartID
	if cartID == "" && !django.HasCartToken(ctx) {
		c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, "")
		if err != nil {
			return nil, out, upstreamErr(err,
				"the cart service is unavailable")
		}
		cartID = c.UUID
	}

	_, err = h.deps.Django.AddCartItem(
		ctx, t.Domain, t.DefaultLocale, cartID, in.ProductID, qty,
	)
	if errors.Is(err, django.ErrCartUnreachable) {
		return nil, out, errors.New("the shopper's cart could not be " +
			"reached, so the product is not in it; tell the shopper " +
			"their cart could not be updated")
	}
	if err != nil {
		return nil, out, upstreamErr(err, fmt.Sprintf(
			"product %d was not found", in.ProductID))
	}
	cartChanged(ctx)

	c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, cartID)
	if err != nil {
		return nil, out, errReadBack
	}
	out = h.cartOut(t, c)
	return h.cartSummary(out), out, nil
}

// errReadBack is a cart change the store accepted whose cart could not
// be read back (a refused token, an outage): the change happened, so
// the model must neither deny it nor repeat it.
var errReadBack = errors.New("the change was sent and the store " +
	"accepted it, but the cart could not be re-read; tell the shopper " +
	"to check their cart, and do not repeat the change")

type UpdateCartItemIn struct {
	CartID   string `json:"cartId,omitempty"`
	ItemID   int64  `json:"itemId" jsonschema:"cart line id from the cart's items"`
	Quantity int    `json:"quantity" jsonschema:"new quantity, 1-999"`
}

func (h *handlers) updateCartItem(
	ctx context.Context, _ *mcp.CallToolRequest, in UpdateCartItemIn,
) (*mcp.CallToolResult, CartOut, error) {
	var out CartOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	if (in.CartID == "" && !django.HasCartToken(ctx)) ||
		in.ItemID <= 0 || in.Quantity <= 0 {
		return nil, out, errors.New(
			"cartId, itemId and a positive quantity are required; use " +
				"remove_cart_item to delete a line")
	}
	if _, err := h.deps.Django.UpdateCartItem(
		ctx, t.Domain, t.DefaultLocale, in.CartID, in.ItemID, in.Quantity,
	); err != nil {
		return nil, out, upstreamErr(err,
			"that cart line was not found; fetch the cart with get_cart")
	}
	cartChanged(ctx)
	c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, in.CartID)
	if err != nil {
		return nil, out, errReadBack
	}
	out = h.cartOut(t, c)
	return h.cartSummary(out), out, nil
}

type RemoveCartItemIn struct {
	CartID string `json:"cartId,omitempty"`
	ItemID int64  `json:"itemId"`
}

func (h *handlers) removeCartItem(
	ctx context.Context, _ *mcp.CallToolRequest, in RemoveCartItemIn,
) (*mcp.CallToolResult, CartOut, error) {
	var out CartOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	if (in.CartID == "" && !django.HasCartToken(ctx)) || in.ItemID <= 0 {
		return nil, out, errors.New("cartId and itemId are required")
	}
	if err := h.deps.Django.RemoveCartItem(
		ctx, t.Domain, t.DefaultLocale, in.CartID, in.ItemID,
	); err != nil {
		return nil, out, upstreamErr(err,
			"that cart line was not found; fetch the cart with get_cart")
	}
	cartChanged(ctx)
	c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, in.CartID)
	if err != nil {
		return nil, out, errReadBack
	}
	out = h.cartOut(t, c)
	return h.cartSummary(out), out, nil
}

type CheckoutLinkIn struct {
	CartID string `json:"cartId,omitempty"`
}

type CheckoutLinkOut struct {
	URL string `json:"url"`
}

func (h *handlers) getCheckoutLink(
	ctx context.Context, _ *mcp.CallToolRequest, in CheckoutLinkIn,
) (*mcp.CallToolResult, CheckoutLinkOut, error) {
	var out CheckoutLinkOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	signedIn := django.HasCartToken(ctx)
	if in.CartID == "" && !signedIn {
		return nil, out, errors.New("cartId is required")
	}
	// Validate the cart exists before handing the shopper a dead link.
	if _, err := h.deps.Django.GetCart(
		ctx, t.Domain, t.DefaultLocale, in.CartID,
	); err != nil {
		return nil, out, upstreamErr(err,
			"that cart no longer exists; create a new one with create_cart")
	}
	// A signed-in shopper's cart is already their session's: the claim
	// link adopts guest carts only, so they go straight to checkout.
	out.URL = storefront.CartClaim(t.Domain, in.CartID)
	if signedIn {
		out.URL = storefront.Checkout(t.Domain)
	}
	return textResult(
		"Give the shopper this link to review the cart and pay on the "+
			"store's checkout (address, delivery and payment are chosen "+
			"there): %s", out.URL,
	), out, nil
}

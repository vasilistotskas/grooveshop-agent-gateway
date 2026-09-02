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
	PromotionDiscount  string   `json:"promotionDiscount,omitempty" jsonschema:"promotion/coupon discount subtracted from total at checkout"`
	TotalDiscountValue string   `json:"totalDiscountValue,omitempty" jsonschema:"product markdown savings already included in item prices"`
	AppliedCouponCodes []string `json:"appliedCouponCodes,omitempty"`
	FreeShipping       bool     `json:"freeShipping,omitempty" jsonschema:"a promotion grants free delivery on this cart"`
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
		AppliedCouponCodes: c.AppliedCouponCodes,
		FreeShipping:       c.PromotionFreeShipping,
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
	discountNote := ""
	if out.PromotionDiscount != "" {
		discountNote = fmt.Sprintf(" A promotion discount of %s %s",
			out.PromotionDiscount, out.Currency)
		if len(out.AppliedCouponCodes) > 0 {
			discountNote += fmt.Sprintf(" (coupon %s)",
				strings.Join(out.AppliedCouponCodes, ", "))
		}
		discountNote += " applies at checkout."
	}
	if out.FreeShipping {
		discountNote += " Shipping is free on this cart."
	}
	return textResult(
		"Cart %s holds %d items totalling %s %s (VAT included).%s Persist "+
			"cartId for later calls; hand the shopper get_checkout_link "+
			"when they're ready to pay.",
		out.CartID, out.TotalItems, out.Total, out.Currency, discountNote,
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
	return textResult(
		"Created cart %s. Persist this cartId — it identifies the "+
			"shopper's cart in every subsequent call.", out.CartID,
	), out, nil
}

type GetCartIn struct {
	CartID string `json:"cartId" jsonschema:"cart id from create_cart or add_to_cart"`
}

func (h *handlers) getCart(
	ctx context.Context, _ *mcp.CallToolRequest, in GetCartIn,
) (*mcp.CallToolResult, CartOut, error) {
	var out CartOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	if in.CartID == "" {
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

	cartID := in.CartID
	if cartID == "" {
		c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, "")
		if err != nil {
			return nil, out, upstreamErr(err,
				"the cart service is unavailable")
		}
		cartID = c.UUID
	}

	if _, err := h.deps.Django.AddCartItem(
		ctx, t.Domain, t.DefaultLocale, cartID, in.ProductID, qty,
	); err != nil {
		return nil, out, upstreamErr(err, fmt.Sprintf(
			"product %d was not found", in.ProductID))
	}

	c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, cartID)
	if err != nil {
		return nil, out, upstreamErr(err, "the cart service is unavailable")
	}
	out = h.cartOut(t, c)
	return h.cartSummary(out), out, nil
}

type UpdateCartItemIn struct {
	CartID   string `json:"cartId"`
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
	if in.CartID == "" || in.ItemID <= 0 || in.Quantity <= 0 {
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
	c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, in.CartID)
	if err != nil {
		return nil, out, upstreamErr(err, "the cart service is unavailable")
	}
	out = h.cartOut(t, c)
	return h.cartSummary(out), out, nil
}

type RemoveCartItemIn struct {
	CartID string `json:"cartId"`
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
	if in.CartID == "" || in.ItemID <= 0 {
		return nil, out, errors.New("cartId and itemId are required")
	}
	if err := h.deps.Django.RemoveCartItem(
		ctx, t.Domain, t.DefaultLocale, in.CartID, in.ItemID,
	); err != nil {
		return nil, out, upstreamErr(err,
			"that cart line was not found; fetch the cart with get_cart")
	}
	c, err := h.deps.Django.GetCart(ctx, t.Domain, t.DefaultLocale, in.CartID)
	if err != nil {
		return nil, out, upstreamErr(err, "the cart service is unavailable")
	}
	out = h.cartOut(t, c)
	return h.cartSummary(out), out, nil
}

type CheckoutLinkIn struct {
	CartID string `json:"cartId"`
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
	if in.CartID == "" {
		return nil, out, errors.New("cartId is required")
	}
	// Validate the cart exists before handing the shopper a dead link.
	if _, err := h.deps.Django.GetCart(
		ctx, t.Domain, t.DefaultLocale, in.CartID,
	); err != nil {
		return nil, out, upstreamErr(err,
			"that cart no longer exists; create a new one with create_cart")
	}
	out.URL = storefront.CartClaim(t.Domain, in.CartID)
	return textResult(
		"Give the shopper this link to review the cart and pay on the "+
			"store's checkout (address, delivery and payment are chosen "+
			"there): %s", out.URL,
	), out, nil
}

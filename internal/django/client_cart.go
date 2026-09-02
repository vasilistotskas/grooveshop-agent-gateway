package django

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// cartHeaders builds the per-call header set for cart operations. The
// X-Cart-Id UUID is the cart's identity; X-Internal-Gateway lets Django's
// anon cart throttle key on the cart instead of the gateway pod IP.
func (c *Client) cartHeaders(cartID string) map[string]string {
	h := map[string]string{"X-Internal-Gateway": c.internalSecret}
	if cartID != "" {
		h["X-Cart-Id"] = cartID
	}
	return h
}

// GetCart fetches the cart identified by cartID; with an empty id Django
// auto-creates a fresh guest cart and returns its UUID.
func (c *Client) GetCart(
	ctx context.Context, host, lang, cartID string,
) (*Cart, error) {
	var out Cart
	err := c.get(ctx, request{
		path:     "/cart",
		host:     host,
		language: lang,
		headers:  c.cartHeaders(cartID),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AddCartItem adds a product (or increments its line) on the cart.
func (c *Client) AddCartItem(
	ctx context.Context, host, lang, cartID string, productID int64, qty int,
) (*CartItem, error) {
	var out CartItem
	err := c.send(ctx, http.MethodPost, request{
		path:     "/cart/item",
		host:     host,
		language: lang,
		headers:  c.cartHeaders(cartID),
		body: map[string]any{
			"product":  productID,
			"quantity": qty,
		},
		out: &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateCartItem(
	ctx context.Context, host, lang, cartID string, itemID int64, qty int,
) (*CartItem, error) {
	var out CartItem
	err := c.send(ctx, http.MethodPut, request{
		path:     "/cart/item/{id}",
		rawPath:  fmt.Sprintf("/cart/item/%d", itemID),
		host:     host,
		language: lang,
		headers:  c.cartHeaders(cartID),
		body:     map[string]any{"quantity": qty},
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RemoveCartItem(
	ctx context.Context, host, lang, cartID string, itemID int64,
) error {
	return c.send(ctx, http.MethodDelete, request{
		path:     "/cart/item/{id}",
		rawPath:  fmt.Sprintf("/cart/item/%d", itemID),
		host:     host,
		language: lang,
		headers:  c.cartHeaders(cartID),
	})
}

// ApplyCoupon attaches a discount code to the cart (the store keeps one
// coupon per cart — a new code replaces the previous one) and returns the
// updated cart. A refusal surfaces as *CouponRejectedError carrying the
// machine-readable reason.
func (c *Client) ApplyCoupon(
	ctx context.Context, host, lang, cartID, code string,
) (*Cart, error) {
	var out Cart
	err := c.send(ctx, http.MethodPost, request{
		path:     "/cart/coupon",
		host:     host,
		language: lang,
		headers:  c.cartHeaders(cartID),
		body:     map[string]any{"code": code},
		out:      &out,
	})
	if err != nil {
		var apiErr *APIError
		if errors.Is(err, ErrValidation) && errors.As(err, &apiErr) &&
			apiErr.Reason != "" {
			return nil, &CouponRejectedError{
				Reason: apiErr.Reason, Detail: apiErr.Detail,
			}
		}
		return nil, err
	}
	return &out, nil
}

// RemoveCoupon detaches the applied coupon (a no-op cart comes back
// unchanged) and returns the updated cart.
func (c *Client) RemoveCoupon(
	ctx context.Context, host, lang, cartID string,
) (*Cart, error) {
	var out Cart
	err := c.send(ctx, http.MethodDelete, request{
		path:     "/cart/coupon",
		host:     host,
		language: lang,
		headers:  c.cartHeaders(cartID),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderByUUID fetches an order through the guest flow: possession of the
// unguessable UUID in the path IS the authorization.
func (c *Client) OrderByUUID(
	ctx context.Context, host, lang, orderUUID string,
) (*Order, error) {
	var out Order
	err := c.get(ctx, request{
		path:     "/order/uuid/{uuid}",
		rawPath:  "/order/uuid/" + url.PathEscape(orderUUID),
		host:     host,
		language: lang,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateProductAlert subscribes an email to restock/price-drop alerts.
// Duplicate subscriptions surface as ErrConflict.
func (c *Client) CreateProductAlert(
	ctx context.Context, host, lang, kind string,
	productID int64, email, targetPrice string,
) (*ProductAlert, error) {
	body := map[string]any{
		"kind":    kind,
		"product": productID,
		"email":   email,
	}
	if targetPrice != "" {
		body["targetPrice"] = targetPrice
	}
	var out ProductAlert
	err := c.send(ctx, http.MethodPost, request{
		path:     "/product/alert",
		host:     host,
		language: lang,
		headers:  map[string]string{"X-Internal-Gateway": c.internalSecret},
		body:     body,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

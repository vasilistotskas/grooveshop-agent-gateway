package django

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"
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

// cartTarget picks how a call on an allowlisted cart route names its
// cart, and the cart id the one that answers must be (servedNamed). A
// signed-in shopper's cart token (WithCartToken) wins over cartID and is
// sent instead of X-Cart-Id. The served cart is then trusted, not
// compared: Django resolves the token to exactly that shopper's cart or
// refuses it with 401, and never serves a guest cart in its place, so
// the cart that answers is the one the shopper sees. Comparing it with
// cartID would refuse their own cart whenever the model names a stale
// guest id. Stock, order and payment calls use cartHeaders and never
// carry the token: Django refuses it there.
func (c *Client) cartTarget(
	ctx context.Context, cartID string,
) (headers map[string]string, named string, err error) {
	tok, ok := cartTokenFrom(ctx)
	if !ok {
		return c.cartHeaders(cartID), cartID, nil
	}
	// A refused token stays refused for the turn; asking again would
	// only spend another round trip on the same 401.
	if tok.Refused() {
		return nil, "", ErrCartTokenRefused
	}
	h := c.cartHeaders("")
	h["X-Cart-Token"] = tok.value
	return h, "", nil
}

// cartRefusal maps a 401 on a call carrying a cart token to
// ErrCartTokenRefused and records it on the token. The call carries no
// other credential, so a 401 there can only be the token: Django's
// cart_token_invalid (unknown, expired or revoked).
func cartRefusal(ctx context.Context, err error) error {
	tok, ok := cartTokenFrom(ctx)
	if !ok || !errors.Is(err, ErrUnauthorized) {
		return err
	}
	tok.refused.Store(true)
	return ErrCartTokenRefused
}

// servedNamed checks that Django acted on the cart the call named, not
// on the new guest cart it serves in place of one the caller cannot
// reach. An empty name asks for that new cart.
func servedNamed(named, served string) error {
	if named == "" {
		return nil
	}
	n, nerr := uuid.Parse(named)
	s, serr := uuid.Parse(served)
	if nerr != nil || serr != nil || n != s {
		return ErrCartUnreachable
	}
	return nil
}

// Request bodies are typed so the consumer contract
// (contract/django-consumer.openapi.json) is generated from them.
type cartItemCreate struct {
	Product     int64        `json:"product"`
	Quantity    int          `json:"quantity"`
	Attribution *Attribution `json:"attribution,omitempty"`
}

type cartItemUpdate struct {
	Quantity int `json:"quantity"`
}

type couponApply struct {
	Code string `json:"code"`
}

type productAlertCreate struct {
	Kind        string      `json:"kind"`
	Product     int64       `json:"product"`
	Email       string      `json:"email" contract:"format=email"`
	TargetPrice json.Number `json:"targetPrice,omitempty" contract:"format=double"`
}

// GetCart fetches the cart identified by cartID; with an empty id Django
// auto-creates a fresh guest cart and returns its UUID.
func (c *Client) GetCart(
	ctx context.Context, host, lang, cartID string,
) (*Cart, error) {
	headers, named, err := c.cartTarget(ctx, cartID)
	if err != nil {
		return nil, err
	}
	var out Cart
	err = c.get(ctx, request{
		path:     "/cart",
		host:     host,
		language: lang,
		headers:  headers,
		out:      &out,
	})
	if err != nil {
		return nil, cartRefusal(ctx, err)
	}
	if err := servedNamed(named, out.UUID); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddCartItem adds a product (or increments its line) on the cart.
// Django records attribution as the cart's source on the add that takes
// it from no lines to some, and ignores it on later adds; nil sends none.
func (c *Client) AddCartItem(
	ctx context.Context, host, lang, cartID string, productID int64, qty int,
	attribution *Attribution,
) (*CartItem, error) {
	headers, named, err := c.cartTarget(ctx, cartID)
	if err != nil {
		return nil, err
	}
	body := cartItemCreate{
		Product: productID, Quantity: qty, Attribution: attribution,
	}
	var out CartItem
	err = c.send(ctx, http.MethodPost, request{
		path:     "/cart/item",
		host:     host,
		language: lang,
		headers:  headers,
		body:     body,
		out:      &out,
	})
	if err != nil {
		return nil, cartRefusal(ctx, err)
	}
	if err := servedNamed(named, out.CartID); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateCartItem(
	ctx context.Context, host, lang, cartID string, itemID int64, qty int,
) (*CartItem, error) {
	headers, named, err := c.cartTarget(ctx, cartID)
	if err != nil {
		return nil, err
	}
	var out CartItem
	err = c.send(ctx, http.MethodPut, request{
		path:     "/cart/item/{id}",
		rawPath:  fmt.Sprintf("/cart/item/%d", itemID),
		host:     host,
		language: lang,
		headers:  headers,
		body:     cartItemUpdate{Quantity: qty},
		out:      &out,
	})
	if err != nil {
		return nil, cartRefusal(ctx, err)
	}
	if err := servedNamed(named, out.CartID); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RemoveCartItem(
	ctx context.Context, host, lang, cartID string, itemID int64,
) error {
	headers, _, err := c.cartTarget(ctx, cartID)
	if err != nil {
		return err
	}
	return cartRefusal(ctx, c.send(ctx, http.MethodDelete, request{
		path:     "/cart/item/{id}",
		rawPath:  fmt.Sprintf("/cart/item/%d", itemID),
		host:     host,
		language: lang,
		headers:  headers,
	}))
}

// ApplyCoupon attaches a discount code to the cart (the store keeps one
// coupon per cart — a new code replaces the previous one) and returns the
// updated cart. A refusal surfaces as *CouponRejectedError carrying the
// machine-readable reason.
func (c *Client) ApplyCoupon(
	ctx context.Context, host, lang, cartID, code string,
) (*Cart, error) {
	headers, named, err := c.cartTarget(ctx, cartID)
	if err != nil {
		return nil, err
	}
	var out Cart
	err = c.send(ctx, http.MethodPost, request{
		path:     "/cart/coupon",
		host:     host,
		language: lang,
		headers:  headers,
		body:     couponApply{Code: code},
		out:      &out,
	})
	if err != nil {
		err = cartRefusal(ctx, err)
		apiErr, ok := errors.AsType[*APIError](err)
		if ok && errors.Is(err, ErrValidation) && apiErr.Reason != "" {
			return nil, &CouponRejectedError{
				Reason: apiErr.Reason, Detail: apiErr.Detail,
			}
		}
		return nil, err
	}
	if err := servedNamed(named, out.UUID); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveCoupon detaches the applied coupon (a no-op cart comes back
// unchanged) and returns the updated cart.
func (c *Client) RemoveCoupon(
	ctx context.Context, host, lang, cartID string,
) (*Cart, error) {
	headers, named, err := c.cartTarget(ctx, cartID)
	if err != nil {
		return nil, err
	}
	var out Cart
	err = c.send(ctx, http.MethodDelete, request{
		path:     "/cart/coupon",
		host:     host,
		language: lang,
		headers:  headers,
		out:      &out,
	})
	if err != nil {
		return nil, cartRefusal(ctx, err)
	}
	if err := servedNamed(named, out.UUID); err != nil {
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
	productID int64, email string, targetPrice json.Number,
) (*ProductAlert, error) {
	var out ProductAlert
	err := c.send(ctx, http.MethodPost, request{
		path:     "/product/alert",
		host:     host,
		language: lang,
		headers:  map[string]string{"X-Internal-Gateway": c.internalSecret},
		body: productAlertCreate{
			Kind:        kind,
			Product:     productID,
			Email:       email,
			TargetPrice: targetPrice,
		},
		out: &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

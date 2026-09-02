package django

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// StockShortfall describes a failed stock reservation (HTTP 409). It is a
// structured outcome, not a transport failure: agents relay the failed
// lines so the shopper can adjust quantities.
type StockShortfall struct {
	Detail      string `json:"detail"`
	FailedItems []struct {
		ProductID   int64  `json:"productId"`
		ProductName string `json:"productName"`
		Available   int    `json:"available"`
		Requested   int    `json:"requested"`
	} `json:"failedItems"`
}

func (s *StockShortfall) Error() string {
	return fmt.Sprintf("django: stock shortfall on %d item(s)",
		len(s.FailedItems))
}

func (s *StockShortfall) Unwrap() error { return ErrConflict }

type ReserveStockResult struct {
	ReservationIDs []int64 `json:"reservationIds"`
	Message        string  `json:"message"`
}

// ReserveStock holds the cart's stock for 15 minutes ahead of order
// creation. A 409 whose body lists the failed lines decodes into
// *StockShortfall; any other failure is the usual *APIError.
func (c *Client) ReserveStock(
	ctx context.Context, host, lang, cartID string,
) (*ReserveStockResult, error) {
	var out ReserveStockResult
	err := c.send(ctx, http.MethodPost, request{
		path:     "/cart/reserve-stock",
		host:     host,
		language: lang,
		headers:  c.cartHeaders(cartID),
		out:      &out,
	})
	if err != nil {
		var apiErr *APIError
		if errors.Is(err, ErrConflict) && errors.As(err, &apiErr) {
			var shortfall StockShortfall
			if json.Unmarshal(apiErr.Body, &shortfall) == nil &&
				len(shortfall.FailedItems) > 0 {
				return nil, &shortfall
			}
		}
		return nil, err
	}
	return &out, nil
}

// OrderCreate is the guest checkout payload (camelCase on the wire). The
// cart is taken from the X-Cart-Id header, never the body.
type OrderCreate struct {
	PayWayID     int64  `json:"payWayId"`
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	Street       string `json:"street"`
	StreetNumber string `json:"streetNumber,omitempty"`
	City         string `json:"city"`
	Zipcode      string `json:"zipcode"`
	CountryID    string `json:"countryId"`
	DocumentType string `json:"documentType,omitempty"`

	ShippingProviderCode string `json:"shippingProviderCode,omitempty"`
	ShippingKind         string `json:"shippingKind,omitempty"`
	AcsStationExternalID string `json:"acsStationExternalId,omitempty"`
	AcsStationBranch     string `json:"acsStationBranch,omitempty"`
	BoxnowLockerID       string `json:"boxnowLockerId,omitempty"`
	BoxnowCompartmentSz  int    `json:"boxnowCompartmentSize,omitempty"`

	CustomerNotes string `json:"customerNotes,omitempty"`
}

// CreateOrder places a guest order from the cart. Never retried: the
// caller owns idempotency (checkout sessions guard completion).
func (c *Client) CreateOrder(
	ctx context.Context, host, lang, cartID string, body OrderCreate,
) (*Order, error) {
	var out Order
	err := c.send(ctx, http.MethodPost, request{
		path:     "/order",
		host:     host,
		language: lang,
		headers:  c.cartHeaders(cartID),
		body:     body,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type CheckoutSessionResult struct {
	SessionID   string `json:"sessionId"`
	CheckoutURL string `json:"checkoutUrl"`
	Status      string `json:"status"`
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
	Provider    string `json:"provider"`
}

// CreateOrderCheckoutSession mints the hosted payment session for an
// online order (Viva Smart Checkout URL; Stripe hosted checkout). Guest
// authorization rides the ?uuid= query parameter.
func (c *Client) CreateOrderCheckoutSession(
	ctx context.Context, host, lang string,
	orderID int64, orderUUID, successURL, cancelURL string,
) (*CheckoutSessionResult, error) {
	var out CheckoutSessionResult
	err := c.send(ctx, http.MethodPost, request{
		path:     "/order/{id}/create_checkout_session",
		rawPath:  fmt.Sprintf("/order/%d/create_checkout_session", orderID),
		host:     host,
		language: lang,
		query:    url.Values{"uuid": {orderUUID}},
		body: map[string]any{
			"successUrl": successURL,
			"cancelUrl":  cancelURL,
		},
		out: &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PayWayByID resolves one payment method (checkout needs its provider
// code and fee).
func (c *Client) PayWayByID(
	ctx context.Context, host, lang string, id int64,
) (*PayWay, error) {
	page, err := c.PayWays(ctx, host, lang, "", "")
	if err != nil {
		return nil, err
	}
	for i := range page.Results {
		if page.Results[i].ID == id {
			return &page.Results[i], nil
		}
	}
	return nil, errors.Join(ErrNotFound,
		fmt.Errorf("pay way %d not found", id))
}

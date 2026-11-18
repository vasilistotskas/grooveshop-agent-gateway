package django

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// QuoteRequest is what a checkout has chosen so far
// (CartQuoteRequestRequest). Every member is optional: Django prices a
// delivery once ShippingKind and CountryID are both given, and leaves
// null every row that depends on a choice not made. Send what the order
// will carry, the email included, so the quote is the total the order is
// created at.
type QuoteRequest struct {
	PayWayID             int64  `json:"payWayId,omitempty"`
	ShippingKind         string `json:"shippingKind,omitempty"`
	ShippingProviderCode string `json:"shippingProviderCode,omitempty"`
	CountryID            string `json:"countryId,omitempty"`
	RegionID             string `json:"regionId,omitempty"`
	Email                string `json:"email,omitempty" contract:"format=email"`
}

// Quote is what checkout charges for a cart with the choices of its
// QuoteRequest (CartQuote): the one computation order creation runs. A
// row that depends on a choice not made is nil: Shipping without a
// delivery, PaymentMethodFee without a pay way, ExpectedTotal without
// both.
type Quote struct {
	ItemsSubtotal     json.Number          `json:"itemsSubtotal"`
	DiscountTotal     json.Number          `json:"discountTotal"`
	AppliedPromotions []AppliedPromotion   `json:"appliedPromotions"`
	BlockingCoupon    *QuoteBlockingCoupon `json:"blockingCoupon"`
	Shipping          *QuoteShipping       `json:"shipping"`
	PaymentMethodFee  *QuotePaymentFee     `json:"paymentMethodFee"`
	Currency          string               `json:"currency"`
	ExpectedTotal     *ExpectedTotal       `json:"expectedTotal"`
	// ShippingOptions is every delivery the store offers to the
	// request's country, priced as an order would be; nil without a
	// country.
	ShippingOptions []QuoteShippingOption `json:"shippingOptions" contract:"nullable"`
}

// QuoteBlockingCoupon is an attached coupon order creation refuses the
// cart for. Reason is in the coupon rejection vocabulary
// (discount_code_expired, ...); its discount is not in the quote.
type QuoteBlockingCoupon struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// QuoteShipping is the chosen delivery's price. WaivedBy says what made
// it free: FreeShippingByPromotion or FreeShippingByThreshold, empty when
// it is charged or simply costs nothing.
type QuoteShipping struct {
	Price    json.Number `json:"price"`
	WaivedBy string      `json:"waivedBy" contract:"nullable,untyped"`
}

// QuotePaymentFee is the chosen pay way's fee for this cart: 0 when it
// has none or the cart reached its free threshold (Waived).
type QuotePaymentFee struct {
	Amount json.Number `json:"amount"`
	Waived bool        `json:"waived"`
}

// QuoteShippingOption is one delivery the store offers, at the price
// this cart would be charged for it. An option the cart is too heavy or
// too large for is listed, and refused if chosen (FitsCart).
type QuoteShippingOption struct {
	ProviderCode      string      `json:"providerCode"`
	ProviderName      string      `json:"providerName"`
	Kind              string      `json:"kind"`
	Price             json.Number `json:"price"`
	ExceedsMaxWeight  bool        `json:"exceedsMaxWeight"`
	ExceedsParcelSize bool        `json:"exceedsParcelSize"`
}

// FitsCart reports whether Django would ship the cart this way.
func (o QuoteShippingOption) FitsCart() bool {
	return !o.ExceedsMaxWeight && !o.ExceedsParcelSize
}

// ExpectedTotal is an order total in whole minor units, which order
// creation compares exactly with the amount due it computes.
type ExpectedTotal struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// The waivedBy values of a quoted delivery.
const (
	FreeShippingByPromotion = "promotion"
	FreeShippingByThreshold = "threshold"
)

// Quote prices the cart with the choices made so far. Nothing is
// written: a quote never creates a cart (an unknown one is a 404
// cart_not_found). A choice the store refuses is a 400 whose code says
// which (CodePayWayUnavailable, the ShippingRefusal codes), so the
// caller can drop it and price the rest.
func (c *Client) Quote(
	ctx context.Context, host, lang, cartID string, body QuoteRequest,
) (*Quote, error) {
	headers, _, err := c.cartTarget(ctx, cartID)
	if err != nil {
		return nil, err
	}
	var out Quote
	err = c.send(ctx, http.MethodPost, request{
		path:     "/cart/quote",
		host:     host,
		language: lang,
		headers:  headers,
		body:     body,
		out:      &out,
	})
	if err != nil {
		return nil, cartRefusal(ctx, err)
	}
	return &out, nil
}

// totalChangedBody is the part of Django's 409 total_changed the gateway
// reads: the total the order would have been created at now.
type totalChangedBody struct {
	Error struct {
		Quote struct {
			ExpectedTotal *ExpectedTotal `json:"expectedTotal"`
		} `json:"quote"`
	} `json:"error"`
}

// TotalChangedError is Django's refusal of an order whose expected total
// is not the amount due it computes now (409 total_changed): nothing was
// created. Now is the total it would have been created at, nil when the
// body did not carry one.
type TotalChangedError struct {
	Now *ExpectedTotal
	err error
}

func (e *TotalChangedError) Error() string {
	return fmt.Sprintf("%s: %s", ErrTotalChanged, e.err)
}

func (e *TotalChangedError) Unwrap() []error {
	return []error{ErrTotalChanged, e.err}
}

// totalChanged reads a 409 total_changed, or reports false.
func totalChanged(err error) (*TotalChangedError, bool) {
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok || apiErr.Code != CodeTotalChanged {
		return nil, false
	}
	var body totalChangedBody
	_ = json.Unmarshal(apiErr.Body, &body)
	return &TotalChangedError{Now: body.Error.Quote.ExpectedTotal, err: err},
		true
}

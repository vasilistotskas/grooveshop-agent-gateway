package django

import (
	"context"
	"encoding/json"
	"net/http"
)

// QuoteRequest is what a checkout has chosen so far
// (CartQuoteRequestRequest). Every member is optional: Django prices a
// delivery once ShippingKind and CountryID are both given, and leaves
// null every row that depends on a choice not made. Send what the order
// will carry, the email included, so the quote is the total the order is
// created at, and its CartDigest of the cart the order is made of, the
// pickup point included.
type QuoteRequest struct {
	PayWayID             int64  `json:"payWayId,omitempty"`
	ShippingKind         string `json:"shippingKind,omitempty"`
	ShippingProviderCode string `json:"shippingProviderCode,omitempty"`
	CountryID            string `json:"countryId,omitempty"`
	RegionID             string `json:"regionId,omitempty"`
	Email                string `json:"email,omitempty" contract:"format=email"`
	PickupPoint
}

// PickupPoint is the pickup point a checkout chose, by its carrier's own
// fields (Django's PickupPointSerializer). The quote and order creation
// take the same ones, and Django reads only the chosen carrier's.
type PickupPoint struct {
	AcsStationExternalID string `json:"acsStationExternalId,omitempty"`
	AcsStationBranch     string `json:"acsStationBranch,omitempty"`
	BoxnowLockerID       string `json:"boxnowLockerId,omitempty"`
}

// Quote is what checkout charges for a cart with the choices of its
// QuoteRequest (CartQuote): the one computation order creation runs. A
// row that depends on a choice not made is nil: Shipping without a
// delivery, PaymentMethodFee without a pay way, ExpectedTotal without
// both, CartDigest without a delivery.
type Quote struct {
	ItemsSubtotal     json.Number          `json:"itemsSubtotal"`
	DiscountTotal     json.Number          `json:"discountTotal"`
	AppliedPromotions []QuotePromotion     `json:"appliedPromotions"`
	BlockingCoupon    *QuoteBlockingCoupon `json:"blockingCoupon"`
	Shipping          *QuoteShipping       `json:"shipping"`
	PaymentMethodFee  *QuotePaymentFee     `json:"paymentMethodFee"`
	Currency          string               `json:"currency"`
	ExpectedTotal     *ExpectedTotal       `json:"expectedTotal"`
	// CartDigest is an opaque digest of what an order would be made of:
	// the lines, the promotions and the delivery with its pickup point,
	// not the pay way. Sent back as OrderCreate.ExpectedCartDigest.
	CartDigest string `json:"cartDigest" contract:"nullable"`
	// ShippingOptions is every delivery the store offers to the
	// request's country, priced as an order would be; nil without a
	// country.
	ShippingOptions []QuoteShippingOption `json:"shippingOptions" contract:"nullable"`
}

// QuotePromotion is one offer that took money off the quoted cart, with
// its own amount; the amounts sum to Quote.DiscountTotal. Code is the
// coupon that earned it, empty for an automatic promotion.
type QuotePromotion struct {
	PromotionID int64       `json:"promotionId"`
	Name        string      `json:"name"`
	Code        string      `json:"code" contract:"nullable"`
	Amount      json.Number `json:"amount"`
}

// QuoteBlockingCoupon is an attached coupon order creation refuses the
// cart for. Reason is in the coupon rejection vocabulary
// (discount_code_expired, ...); its discount is not in the quote.
type QuoteBlockingCoupon struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// QuoteShipping is the chosen delivery's price, free when the carrier's
// threshold or a promotion waived it.
type QuoteShipping struct {
	Price json.Number `json:"price"`
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

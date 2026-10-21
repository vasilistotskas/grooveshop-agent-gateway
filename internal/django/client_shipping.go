package django

import (
	"context"
	"net/http"
	"net/url"
)

// ShippingQuery maps to /shipping/options query parameters.
type ShippingQuery struct {
	CountryCode      string
	OrderValueAmount string
	Currency         string
	WeightGrams      string
}

// shippingCartHeaders names the cart a read-only shipping call is judged
// for, as the cart routes do (cartTarget): a chat shopper's cart token
// when the turn carries one, else cartID as X-Cart-Id. Nil when neither
// names a cart, so the call goes out as before.
func (c *Client) shippingCartHeaders(
	ctx context.Context, cartID string,
) (map[string]string, error) {
	if cartID == "" && !HasCartToken(ctx) {
		return nil, nil
	}
	headers, _, err := c.cartTarget(ctx, cartID)
	return headers, err
}

// ShippingOptions lists the store's options for sq. The cart it names
// (shippingCartHeaders) is the one Django checks each option's parcels
// against (ExceedsParcelSize); with none no option is flagged for size.
func (c *Client) ShippingOptions(
	ctx context.Context, host, lang, cartID string, sq ShippingQuery,
) ([]ShippingOption, error) {
	q := url.Values{"countryCode": {sq.CountryCode}}
	if sq.OrderValueAmount != "" {
		q.Set("orderValueAmount", sq.OrderValueAmount)
	}
	if sq.Currency != "" {
		q.Set("currency", sq.Currency)
	}
	if sq.WeightGrams != "" {
		q.Set("weightGrams", sq.WeightGrams)
	}
	headers, err := c.shippingCartHeaders(ctx, cartID)
	if err != nil {
		return nil, err
	}
	var out []ShippingOption
	err = c.get(ctx, request{
		path:     "/shipping/options",
		host:     host,
		language: lang,
		query:    q,
		headers:  headers,
		out:      &out,
	})
	if err != nil {
		return nil, cartRefusal(ctx, err)
	}
	return out, nil
}

func (c *Client) FreeShippingInfo(
	ctx context.Context, host, lang, countryCode string,
) (*FreeShippingInfo, error) {
	var out FreeShippingInfo
	err := c.get(ctx, request{
		path:     "/shipping/free-shipping-info",
		host:     host,
		language: lang,
		query:    url.Values{"countryCode": {countryCode}},
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AcsNearestStations matches by postcode prefix (5→3 digits) with a city
// fallback upstream; coordinates are not supported by the ACS dataset.
// postalCode is required upstream.
func (c *Client) AcsNearestStations(
	ctx context.Context, host, lang, postalCode, city string,
) ([]AcsStation, error) {
	q := url.Values{"postalCode": {postalCode}}
	if city != "" {
		q.Set("city", city)
	}
	var out []AcsStation
	err := c.get(ctx, request{
		path:     "/shipping/acs/stations/nearest",
		host:     host,
		language: lang,
		query:    q,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BoxNowNearestRequest mirrors the lockers/nearest proxy body.
type BoxNowNearestRequest struct {
	City       string `json:"city"`
	Street     string `json:"street"`
	PostalCode string `json:"postalCode"`
}

// BoxNowNearestLocker proxies BoxNow's checkAddressDelivery. POST but
// read-only upstream; throttled at 10/min there, so no transport retry.
// Django asks for a compartment the chat shopper's cart (their cart
// token) fits; with no cart named, the store's default size.
func (c *Client) BoxNowNearestLocker(
	ctx context.Context, host, lang string, body BoxNowNearestRequest,
) (*BoxNowLocker, error) {
	headers, err := c.shippingCartHeaders(ctx, "")
	if err != nil {
		return nil, err
	}
	var out BoxNowLocker
	err = c.send(ctx, http.MethodPost, request{
		path:     "/shipping/boxnow/lockers/nearest",
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

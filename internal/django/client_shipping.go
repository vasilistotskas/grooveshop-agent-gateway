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

func (c *Client) ShippingOptions(
	ctx context.Context, host, lang string, sq ShippingQuery,
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
	var out []ShippingOption
	err := c.get(ctx, request{
		path:     "/shipping/options",
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
func (c *Client) AcsNearestStations(
	ctx context.Context, host, lang, postalCode, city string,
) ([]AcsStation, error) {
	q := url.Values{}
	if postalCode != "" {
		q.Set("postalCode", postalCode)
	}
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
func (c *Client) BoxNowNearestLocker(
	ctx context.Context, host, lang string, body BoxNowNearestRequest,
) (*BoxNowLocker, error) {
	var out BoxNowLocker
	err := c.send(ctx, http.MethodPost, request{
		path:     "/shipping/boxnow/lockers/nearest",
		host:     host,
		language: lang,
		body:     body,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

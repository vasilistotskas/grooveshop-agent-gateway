package django

import (
	"context"
	"fmt"
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

// catalogueSearchSize is the page a catalogue search by id reads: the
// search matches the id as a substring, so a short one can match others
// beside it.
const catalogueSearchSize = "50"

// AcsStationByID reads one ACS station from the store's catalogue by its
// external id and branch (the pair a station is, and an order names),
// ErrNotFound when the catalogue holds none: not synced yet, or a store
// without ACS (whose station routes answer 404).
func (c *Client) AcsStationByID(
	ctx context.Context, host, lang, externalID, branch string,
) (*AcsStation, error) {
	var out Page[AcsStation]
	err := c.get(ctx, request{
		path:     "/shipping/acs/stations",
		host:     host,
		language: lang,
		query: url.Values{
			"search": {externalID}, "pageSize": {catalogueSearchSize},
		},
		out: &out,
	})
	if err != nil {
		return nil, err
	}
	for i := range out.Results {
		st := &out.Results[i]
		if st.ExternalID == externalID && st.BranchCode == branch {
			return st, nil
		}
	}
	return nil, fmt.Errorf("%w: acs station %s/%s", ErrNotFound,
		externalID, branch)
}

// BoxNowCatalogueLocker is a BOX NOW locker as the store's catalogue
// holds it (BoxNowLocker in Django's schema), not as BOX NOW's live
// nearest lookup answers.
type BoxNowCatalogueLocker struct {
	ExternalID   string `json:"externalId"`
	Name         string `json:"name"`
	AddressLine1 string `json:"addressLine1"`
	AddressLine2 string `json:"addressLine2"`
	PostalCode   string `json:"postalCode"`
	CountryCode  string `json:"countryCode"`
}

// BoxNowLockerByID reads one BOX NOW locker from the store's catalogue by
// its id, ErrNotFound when the catalogue holds none (it syncs daily, and
// BOX NOW's own lookup is live).
func (c *Client) BoxNowLockerByID(
	ctx context.Context, host, lang, id string,
) (*BoxNowCatalogueLocker, error) {
	var out Page[BoxNowCatalogueLocker]
	err := c.get(ctx, request{
		path:     "/shipping/boxnow/lockers",
		host:     host,
		language: lang,
		query: url.Values{
			"search": {id}, "pageSize": {catalogueSearchSize},
		},
		out: &out,
	})
	if err != nil {
		return nil, err
	}
	for i := range out.Results {
		if out.Results[i].ExternalID == id {
			return &out.Results[i], nil
		}
	}
	return nil, fmt.Errorf("%w: boxnow locker %s", ErrNotFound, id)
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

// Regions lists a country's delivery regions; a country without any, or
// one Django does not know, answers an empty list.
func (c *Client) Regions(
	ctx context.Context, host, lang, countryCode string,
) ([]Region, error) {
	var out []Region
	err := c.get(ctx, request{
		path: "/region/{alpha}/get_regions_by_country_alpha_2",
		rawPath: "/region/" + url.PathEscape(countryCode) +
			"/get_regions_by_country_alpha_2",
		host:     host,
		language: lang,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

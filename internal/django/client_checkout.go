package django

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
		apiErr, ok := errors.AsType[*APIError](err)
		if ok && errors.Is(err, ErrConflict) {
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
// cart is taken from the X-Cart-Id header, never the body. A home
// delivery's Street is the one street line, its house number after the
// street ("Ερμού 12"), and no streetNumber is sent: Django splits the
// line. A pickup-point order carries the point (PickupPoint) and no
// address: Django delivers it to the point's. Django refuses a line it
// cannot split, or a point it does not know (DestinationRefusedError).
type OrderCreate struct {
	PayWayID  int64  `json:"payWayId"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Email     string `json:"email" contract:"format=email"`
	Phone     string `json:"phone"`
	Street    string `json:"street,omitempty"`
	// AddressLine2 is what the street line does not hold: a flat, a
	// floor, a building.
	AddressLine2 string `json:"addressLine2,omitempty"`
	City         string `json:"city,omitempty"`
	Zipcode      string `json:"zipcode,omitempty"`
	CountryID    string `json:"countryId"`
	// RegionID is required upstream when the country has regions.
	RegionID     string `json:"regionId,omitempty"`
	DocumentType string `json:"documentType,omitempty"`

	ShippingProviderCode string `json:"shippingProviderCode,omitempty"`
	ShippingKind         string `json:"shippingKind,omitempty"`
	PickupPoint

	CustomerNotes string `json:"customerNotes,omitempty"`

	// TermsAccepted: the buyer accepts the store's terms of use and
	// privacy policy. Django records the texts in force with the order.
	TermsAccepted bool `json:"termsAccepted"`

	Attribution *Attribution `json:"attribution,omitempty"`

	// ExpectedTotal is the total the buyer was shown: Django creates the
	// order only at exactly it, else answers 409 total_changed.
	ExpectedTotal ExpectedTotal `json:"expectedTotal"`
	// ExpectedCartDigest is the CartDigest of the quote whose total the
	// buyer was shown: Django creates the order only from exactly the
	// cart that quote priced (its lines, promotions and delivery, with
	// the pickup point of a pickup; not the pay way), else answers 409
	// cart_changed. It
	// catches what the total cannot: a line swapped for one at the same
	// price.
	ExpectedCartDigest string `json:"expectedCartDigest"`

	// AgentCheckout is the checkout session's id. Django believes it
	// beside the internal secret alone, keeps one order per checkout,
	// and answers a repeat with that order (201, the same body), before
	// it judges anything else in the request: so a create whose answer
	// was lost is sent again with the same id.
	AgentCheckout string `json:"agentCheckout" contract:"format=uuid"`
}

// Attribution is the acquisition signal Django classifies into an
// order's or a cart's source (its OrderAttributionInput, the same on
// order create and on add to cart). AgentProtocol is set only for a UCP
// or ACP checkout ("ucp" | "acp"): Django believes it beside the
// internal secret alone, and books the source "AI agent". The other
// fields are the shopper's first touch as the storefront captured it,
// relayed for a chat turn; Django checks their shape, never their
// content.
type Attribution struct {
	UtmSource     string   `json:"utmSource,omitempty"`
	UtmMedium     string   `json:"utmMedium,omitempty"`
	UtmCampaign   string   `json:"utmCampaign,omitempty"`
	ClickIDs      []string `json:"clickIds,omitempty"`
	Referrer      string   `json:"referrer,omitempty"`
	LandingPath   string   `json:"landingPath,omitempty"`
	AgentProtocol string   `json:"agentProtocol,omitempty"`
}

// CreateOrder places a guest order from the cart, once: the caller
// decides on sending it again, which is safe with the same
// AgentCheckout (checkout.Flow). Django's
// refusal of a cart whose earlier order awaits its payment is
// ErrCartHasUnpaidOrder, of a total that is no longer the amount due
// ErrTotalChanged, of a cart that is no longer the one quoted
// ErrCartChanged, and of a street line it cannot split or a pickup point
// it does not know a *DestinationRefusedError.
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
	if apiErr, ok := errors.AsType[*APIError](err); ok &&
		apiErr.Code == CodeCartHasUnpaidOrder {
		return nil, fmt.Errorf("%w: %w", ErrCartHasUnpaidOrder, err)
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		switch apiErr.Code {
		case CodeTotalChanged:
			return nil, fmt.Errorf("%w: %w", ErrTotalChanged, err)
		case CodeCartChanged:
			return nil, fmt.Errorf("%w: %w", ErrCartChanged, err)
		}
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		if refused := destinationRefusal(apiErr); refused != nil {
			return nil, refused
		}
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DestinationRefusedError is Django's refusal of an order's destination
// (400, on one field): on `street`, the one street line, whatever its
// code but a street not sent (blank, required), which is a missing field
// (among them a line it cannot split into the street and its house
// number, CodeStreetNumberMissing; one whose number is a postcode,
// CodeStreetNumberIsPostcode; a street or a number the address rules
// refuse, street_name_missing or the number field's own checks); on the
// carrier's location field, a pickup point its catalogue does not hold
// active (CodePickupPointUnknown), one in another country than the
// order's (CodePickupPointInAnotherCountry) or none named
// (CodePickupPointRequired). Nothing was created. Message is the store's
// own sentence. It unwraps to Django's *APIError, so it is ErrValidation
// too.
type DestinationRefusedError struct {
	Field   string
	Code    string
	Message string
	err     *APIError
}

func (e *DestinationRefusedError) Error() string {
	return fmt.Sprintf("django: %s refused (%s): %s", e.Field, e.Code,
		e.Message)
}

func (e *DestinationRefusedError) Unwrap() error {
	if e.err == nil {
		return ErrValidation
	}
	return e.err
}

// destinationFields are the fields of an order's destination Django
// refuses one on, by the codes it refuses each with: every code on
// `street` (nil), but a street not sent (refusesDestination).
var destinationFields = map[string][]string{
	"street": nil,
	"boxnowLockerId": {CodePickupPointUnknown,
		CodePickupPointInAnotherCountry, CodePickupPointRequired},
	"acsStationExternalId": {CodePickupPointUnknown,
		CodePickupPointInAnotherCountry, CodePickupPointRequired},
	"shippingProviderCode": {CodePickupPointRequired},
}

// refusesDestination reports whether Django's code on field refuses the
// order's destination: on `street`, any code but a street not sent.
func refusesDestination(field, code string) bool {
	if codes := destinationFields[field]; codes != nil {
		return slices.Contains(codes, code)
	}
	switch code {
	case "blank", "required", "null":
		return false
	}
	return true
}

// destinationRefusal is the destination's refusal a 400 carries in its
// `fields`, nil for any other error: the body's own `code` is another
// one whenever another field was refused too.
func destinationRefusal(apiErr *APIError) *DestinationRefusedError {
	if apiErr.Status != http.StatusBadRequest {
		return nil
	}
	var body struct {
		Fields map[string][]struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"fields"`
	}
	// A body that is not a field-error map refuses no destination.
	_ = json.Unmarshal(apiErr.Body, &body)
	for _, field := range slices.Sorted(maps.Keys(destinationFields)) {
		for _, f := range body.Fields[field] {
			if refusesDestination(field, f.Code) {
				return &DestinationRefusedError{Field: field, Code: f.Code,
					Message: f.Message, err: apiErr}
			}
		}
	}
	return nil
}

type CheckoutSessionResult struct {
	CheckoutURL string `json:"checkoutUrl"`
	Status      string `json:"status"`
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
}

// CreateOrderCheckoutSession mints the hosted payment session for an
// online order (Viva Smart Checkout URL; Stripe hosted checkout). Guest
// authorization rides the ?uuid= query parameter. X-Internal-Gateway
// lets Django's anonymous payment throttle key on the order instead of
// the caller the gateway relays, as its cart and order calls do.
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
		headers:  map[string]string{"X-Internal-Gateway": c.internalSecret},
		body: checkoutSessionCreate{
			SuccessURL: successURL,
			CancelURL:  cancelURL,
		},
		out: &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type checkoutSessionCreate struct {
	SuccessURL string `json:"successUrl" contract:"format=uri"`
	CancelURL  string `json:"cancelUrl" contract:"format=uri"`
}

// PayWayByID resolves one payment method (pricing needs its fee). The
// detail endpoint applies the same visibility rules as the list: inactive
// and credential-less methods are a 404.
func (c *Client) PayWayByID(
	ctx context.Context, host, lang string, id int64,
) (*PayWay, error) {
	var out PayWay
	err := c.get(ctx, request{
		path:     "/pay_way/{id}",
		rawPath:  "/pay_way/" + strconv.FormatInt(id, 10),
		host:     host,
		language: lang,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PayWays lists active payment methods, optionally filtered by carrier
// compatibility (providerCode + kind), mirroring the storefront checkout.
func (c *Client) PayWays(
	ctx context.Context, host, lang, shippingProviderCode, shippingKind string,
) (*Page[PayWay], error) {
	// One page holds every method a store could plausibly configure; the
	// paginator's default of 12 would silently hide the rest.
	q := url.Values{"pageSize": {"100"}}
	if shippingProviderCode != "" {
		q.Set("shippingProviderCode", shippingProviderCode)
	}
	if shippingKind != "" {
		q.Set("shippingKind", shippingKind)
	}
	var out Page[PayWay]
	err := c.get(ctx, request{
		path:     "/pay_way",
		host:     host,
		language: lang,
		query:    q,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PlacedOrder reads an order the gateway itself placed, for its own work: a
// platform hears about an order for as long as the order moves, which outlives
// what a guest's UUID opens. Only for a UUID the gateway holds: one a client
// hands it goes through OrderByUUID, so the client gets the guest's limit.
func (c *Client) PlacedOrder(
	ctx context.Context, host, lang, orderUUID string,
) (*Order, error) {
	var out Order
	err := c.get(ctx, request{
		path:     "/agent/internal/order/{uuid}",
		rawPath:  "/agent/internal/order/" + url.PathEscape(orderUUID),
		host:     host,
		language: lang,
		headers:  map[string]string{"X-Internal-Token": c.internalSecret},
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderByUUID fetches an order through the guest flow: possession of the
// unguessable UUID in the path IS the authorization, for as long as Django
// lets a guest's UUID open the order.
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

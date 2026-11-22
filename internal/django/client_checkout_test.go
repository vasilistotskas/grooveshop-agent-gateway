package django

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReserveStockDecodesResult(t *testing.T) {
	var gotMethod, gotCartID, gotGateway string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/cart/reserve-stock", r.URL.Path)
			gotMethod = r.Method
			gotCartID = r.Header.Get("X-Cart-Id")
			gotGateway = r.Header.Get("X-Internal-Gateway")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "reserve_stock.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	res, err := c.ReserveStock(t.Context(),
		"shop.example.test", "el", "cart-uuid")
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "cart-uuid", gotCartID)
	assert.Equal(t, "test-secret", gotGateway)
	assert.Equal(t, []int64{77, 78}, res.ReservationIDs)
}

// A 409 that lists the failed lines is a structured outcome agents relay
// to the shopper; it must unwrap to ErrConflict for generic handlers.
func TestReserveStockShortfall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write(fixture(t, "reserve_stock_shortfall.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ReserveStock(t.Context(),
		"shop.example.test", "el", "cart-uuid")
	require.Error(t, err)

	var shortfall *StockShortfall
	require.ErrorAs(t, err, &shortfall)
	require.Len(t, shortfall.FailedItems, 1)
	assert.Equal(t, "Θήκη Κινητού", shortfall.FailedItems[0].ProductName)
	assert.Equal(t, 1, shortfall.FailedItems[0].Available)
	assert.ErrorIs(t, err, ErrConflict)
}

// A 409 without failed lines is an ordinary conflict, not a shortfall
// with nothing in it.
func TestReserveStockPlainConflictStaysAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail": "Cart is being modified"}`))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ReserveStock(t.Context(),
		"shop.example.test", "el", "cart-uuid")
	require.Error(t, err)

	var shortfall *StockShortfall
	assert.NotErrorAs(t, err, &shortfall)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "Cart is being modified", apiErr.Detail)
	assert.ErrorIs(t, err, ErrConflict)
}

// Mutations never retry at the transport level: a reservation that timed
// out must not be re-sent and hold stock twice.
func TestReserveStockDoesNotRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusBadGateway)
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ReserveStock(t.Context(),
		"shop.example.test", "el", "cart-uuid")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUpstreamDown)
	assert.Equal(t, 1, calls)
}

// The attribution rides the body camelCase, so Django books the order
// with the agent source type; without one the key is left out entirely.
// termsAccepted is always sent, so a false is never mistaken for absent.
func TestCreateOrderSendsAttribution(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/order", r.URL.Path)
			assert.Equal(t, "cart-uuid", r.Header.Get("X-Cart-Id"))
			got = nil
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "order_by_uuid.json"))
		}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	_, err := c.CreateOrder(t.Context(),
		"shop.example.test", "el", "cart-uuid", OrderCreate{
			PayWayID:    1,
			Attribution: &Attribution{AgentProtocol: "acp"},
		})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"agentProtocol": "acp"},
		got["attribution"])
	assert.Equal(t, false, got["termsAccepted"])

	_, err = c.CreateOrder(t.Context(),
		"shop.example.test", "el", "cart-uuid", OrderCreate{PayWayID: 1})
	require.NoError(t, err)
	assert.NotContains(t, got, "attribution")
}

// The checkout's id rides the body as agentCheckout, which Django keeps
// one order per, and the answer names the checkout that placed it. The
// call is sent once whatever the outcome: whether to send it again is
// the checkout flow's decision, not the transport's, so no
// Idempotency-Key is set for net/http to replay it by.
func TestCreateOrderNamesItsCheckoutAndIsSentOnce(t *testing.T) {
	const checkoutID = "3f1c2a9e-8d5b-4c7a-9e2f-1b6d0a4c8e75"
	var got map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			calls++
			assert.Empty(t, r.Header.Get("Idempotency-Key"))
			got = nil
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			if calls > 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(bytes.Replace(fixture(t, "order_by_uuid.json"),
				[]byte(`"agentCheckout":null`),
				[]byte(`"agentCheckout":"`+checkoutID+`"`), 1))
		}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	order, err := c.CreateOrder(t.Context(), "shop.example.test", "el",
		"cart-uuid", OrderCreate{PayWayID: 1, AgentCheckout: checkoutID})
	require.NoError(t, err)
	assert.Equal(t, checkoutID, got["agentCheckout"])
	assert.Equal(t, checkoutID, order.AgentCheckout)

	_, err = c.CreateOrder(t.Context(), "shop.example.test", "el",
		"cart-uuid", OrderCreate{PayWayID: 1, AgentCheckout: checkoutID})
	require.ErrorIs(t, err, ErrUpstreamDown)
	assert.Equal(t, 2, calls, "a 5xx is answered, not sent again")
}

// Django's refusal of a cart whose earlier order awaits its payment is
// one sentinel, and still the *APIError with Django's code and sentence.
// Another 400 is not it.
func TestCreateOrderNamesTheUnpaidCartRefusal(t *testing.T) {
	refused := func(body []byte) error {
		srv := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(body)
			}))
		defer srv.Close()
		_, err := newTestClient(t, srv.URL).CreateOrder(
			t.Context(), "shop.example.test", "el", "cart-uuid",
			OrderCreate{PayWayID: 1})
		return err
	}

	err := refused(fixture(t, "order_cart_has_unpaid_order.json"))

	require.ErrorIs(t, err, ErrCartHasUnpaidOrder)
	require.ErrorIs(t, err, ErrValidation)
	apiErr, ok := errors.AsType[*APIError](err)
	require.True(t, ok)
	assert.Equal(t, CodeCartHasUnpaidOrder, apiErr.Code)
	assert.Contains(t, apiErr.Detail,
		"An order placed from this cart is already waiting for its payment")

	err = refused([]byte(`{"detail": "Invalid order data", ` +
		`"code": "invalid_order_data"}`))
	require.ErrorIs(t, err, ErrValidation)
	assert.NotErrorIs(t, err, ErrCartHasUnpaidOrder)
}

// The order carries one street line and no street number: Django splits
// the line, and a line it cannot split is its own refusal, read from the
// field it names whatever the body's own code is.
func TestCreateOrderSendsOneStreetLineAndNamesItsRefusal(t *testing.T) {
	var got map[string]any
	var answer []byte
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = nil
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(answer)
		}))
	defer srv.Close()
	create := func() error {
		_, err := newTestClient(t, srv.URL).CreateOrder(
			t.Context(), "shop.example.test", "el", "cart-uuid",
			OrderCreate{PayWayID: 1, Street: "Ερμού 12",
				AddressLine2: "3ος όροφος"})
		return err
	}

	answer = fixture(t, "order_street_number_missing.json")
	err := create()
	assert.Equal(t, "Ερμού 12", got["street"])
	assert.Equal(t, "3ος όροφος", got["addressLine2"])
	assert.NotContains(t, got, "streetNumber")
	refused, ok := errors.AsType[*DestinationRefusedError](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, CodeStreetNumberMissing, refused.Code)
	assert.Contains(t, refused.Message, "Ermou 12")
	require.ErrorIs(t, err, ErrValidation)

	// Another field refused beside it: the body's code is invalid.
	answer = []byte(`{"street": ["x"], "phone": ["y"], "code": "invalid", ` +
		`"fields": {"phone": [{"message": "y", "code": "invalid"}], ` +
		`"street": [{"message": "x", "code": "street_number_is_postcode"}]}}`)
	refused, ok = errors.AsType[*DestinationRefusedError](create())
	require.True(t, ok)
	assert.Equal(t, CodeStreetNumberIsPostcode, refused.Code)

	// Any other refusal of the line is the line's, with Django's code.
	answer = []byte(`{"street": ["x"], "code": "invalid", "fields": ` +
		`{"street": [{"message": "x", "code": "street_name_missing"}]}}`)
	refused, ok = errors.AsType[*DestinationRefusedError](create())
	require.True(t, ok)
	assert.Equal(t, "street_name_missing", refused.Code)

	answer = []byte(`{"street": ["x"], "code": "blank", ` +
		`"fields": {"street": [{"message": "x", "code": "blank"}]}}`)
	err = create()
	require.ErrorIs(t, err, ErrValidation)
	assert.NotErrorAs(t, err, new(*DestinationRefusedError),
		"a blank street is not a line it could not split")
}

// A pickup-point order carries the point under its carrier's field and no
// address: Django delivers it to the point's. A point the catalogue does
// not hold is its own refusal, naming the field.
func TestCreateOrderSendsThePickupPointAndNamesItsRefusal(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = nil
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(fixture(t, "order_pickup_point_unknown.json"))
		}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).CreateOrder(t.Context(),
		"shop.example.test", "el", "cart-uuid", OrderCreate{
			PayWayID: 1, CountryID: "GR", ShippingKind: "pickup_point",
			ShippingProviderCode: ShippingProviderBoxNow,
			BoxnowLockerID:       "25",
		})
	assert.Equal(t, "25", got["boxnowLockerId"])
	assert.Equal(t, "pickup_point", got["shippingKind"])
	for _, field := range []string{"street", "city", "zipcode",
		"streetNumber"} {
		assert.NotContains(t, got, field)
	}
	refused, ok := errors.AsType[*DestinationRefusedError](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, "boxnowLockerId", refused.Field)
	assert.Equal(t, CodePickupPointUnknown, refused.Code)
}

// A point in another country than the order's is a refusal of the
// destination too, under its own code.
func TestCreateOrderNamesAPointInAnotherCountry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(fixture(t,
				"order_pickup_point_in_another_country.json"))
		}))
	defer srv.Close()
	_, err := newTestClient(t, srv.URL).CreateOrder(t.Context(),
		"shop.example.test", "el", "cart-uuid", OrderCreate{
			PayWayID: 1, CountryID: "CY", ShippingKind: "pickup_point",
			ShippingProviderCode: ShippingProviderBoxNow,
			BoxnowLockerID:       "25",
		})
	refused, ok := errors.AsType[*DestinationRefusedError](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, CodePickupPointInAnotherCountry, refused.Code)
	assert.Equal(t, "boxnowLockerId", refused.Field)
}

// A pickup point is read from the catalogue with Django's exact filters,
// in the order's country: an ACS station by its area code and branch
// (the area code alone names every Smartpoint of the area), a BOX NOW
// locker by its id. Only the row that is exactly the one asked is read,
// and none is ErrNotFound.
func TestPickupPointsAreReadFromTheCatalogueExactly(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Query().Get("externalId") == "none":
				_, _ = w.Write([]byte(`{"count":0,"results":[]}`))
			case r.URL.Path == "/api/v1/shipping/acs/stations":
				// The fixture answers another branch of the area first.
				_, _ = w.Write(fixture(t, "acs_stations_search.json"))
			case r.URL.Path == "/api/v1/shipping/boxnow/lockers":
				_, _ = w.Write(fixture(t, "boxnow_lockers_search.json"))
			}
		}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	st, err := c.AcsStationByID(t.Context(), "shop.example.test", "el",
		"AAT", "502", "GR")
	require.NoError(t, err)
	assert.Equal(t, "502", st.BranchCode)
	assert.Equal(t, "ACS SMARTPOINT LOCKER ΠΕΔΙΟ ΑΡΕΩΣ", st.Name)
	_, err = c.AcsStationByID(t.Context(), "shop.example.test", "el",
		"none", "1", "GR")
	require.ErrorIs(t, err, ErrNotFound)

	l, err := c.BoxNowLockerByID(t.Context(), "shop.example.test", "el",
		"25", "GR")
	require.NoError(t, err)
	assert.Equal(t, "AB Shop & Go - Εξάρχεια", l.Name)
	assert.Equal(t, "Ιπποκράτους 132", l.AddressLine1)
	_, err = c.BoxNowLockerByID(t.Context(), "shop.example.test", "el",
		"none", "GR")
	require.ErrorIs(t, err, ErrNotFound)

	assert.Equal(t, []string{
		"/api/v1/shipping/acs/stations?branch=502&countryCode=GR&externalId=AAT",
		"/api/v1/shipping/acs/stations?branch=1&countryCode=GR&externalId=none",
		"/api/v1/shipping/boxnow/lockers?countryCode=GR&externalId=25",
		"/api/v1/shipping/boxnow/lockers?countryCode=GR&externalId=none",
	}, queries, "one call each")
}

func TestLocalized(t *testing.T) {
	tr := map[string]Translation{
		"en": {Name: "Mug"}, "el": {Name: "Κούπα"}, "de": {Name: "Tasse"},
	}
	assert.Equal(t, "Κούπα", Localized(tr, "el").Name)
	// Missing locale: deterministic fallback, not map-order luck.
	assert.Equal(t, "Tasse", Localized(tr, "fr").Name)
	assert.Empty(t, Localized(map[string]Translation{}, "el").Name)
}

// Django keys its anonymous payment throttle on the order for a call
// that carries the gateway's internal secret, and on the caller's
// address otherwise: every buyer behind one AI platform would share it.
func TestCreateOrderCheckoutSessionNamesTheGateway(t *testing.T) {
	var gotGateway, gotUUID string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/order/684/create_checkout_session",
				r.URL.Path)
			gotGateway = r.Header.Get("X-Internal-Gateway")
			gotUUID = r.URL.Query().Get("uuid")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessionId":"s","checkoutUrl":"https://pay.example.test/s","status":"created","amount":"9.30","currency":"EUR","provider":"viva_wallet"}`))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	res, err := c.CreateOrderCheckoutSession(t.Context(),
		"shop.example.test", "el", 684, "order-uuid",
		"https://shop.example.test/ok", "https://shop.example.test/cancel")
	require.NoError(t, err)

	assert.Equal(t, "test-secret", gotGateway)
	assert.Equal(t, "order-uuid", gotUUID)
	assert.Equal(t, "https://pay.example.test/s", res.CheckoutURL)
}

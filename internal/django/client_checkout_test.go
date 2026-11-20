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

package django

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeQuote(t *testing.T) {
	q := decodeFixture[Quote](t, "cart_quote.json")

	assert.Equal(t, "929.36", q.ItemsSubtotal.String())
	assert.Equal(t, "0", q.DiscountTotal.String())
	assert.Empty(t, q.AppliedPromotions)
	assert.Nil(t, q.BlockingCoupon)
	require.NotNil(t, q.Shipping)
	assert.Equal(t, "4.9", q.Shipping.Price.String())
	assert.Empty(t, q.Shipping.WaivedBy)
	require.NotNil(t, q.PaymentMethodFee)
	assert.Equal(t, "2", q.PaymentMethodFee.Amount.String())
	assert.Equal(t, "EUR", q.Currency)
	assert.Equal(t, &ExpectedTotal{Amount: 93626, Currency: "EUR"},
		q.ExpectedTotal)
	require.Len(t, q.ShippingOptions, 3)
	assert.True(t, q.ShippingOptions[0].FitsCart())
	assert.False(t, q.ShippingOptions[2].FitsCart(), "over the weight cap")
}

// The quote names its cart as every cart call does, carries the choices
// camelCase and leaves out the ones not made: Django prices only what
// was chosen.
func TestQuoteSendsTheChoicesMade(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/cart/quote", r.URL.Path)
			assert.Equal(t, "cart-uuid", r.Header.Get("X-Cart-Id"))
			got = nil
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "cart_quote.json"))
		}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	q, err := c.Quote(t.Context(), "shop.example.test", "el", "cart-uuid",
		QuoteRequest{
			PayWayID: 3, ShippingKind: "home_delivery",
			ShippingProviderCode: "acs", CountryID: "GR",
			Email: "buyer@example.test",
		})
	require.NoError(t, err)
	assert.Equal(t, int64(93626), q.ExpectedTotal.Amount)
	assert.Equal(t, map[string]any{
		"payWayId": float64(3), "shippingKind": "home_delivery",
		"shippingProviderCode": "acs", "countryId": "GR",
		"email": "buyer@example.test",
	}, got)

	_, err = c.Quote(t.Context(), "shop.example.test", "el", "cart-uuid",
		QuoteRequest{})
	require.NoError(t, err)
	assert.Empty(t, got, "nothing chosen, nothing sent")
}

// The expected total always rides the order, so Django creates it only
// at the total the buyer was shown; Django's refusal of another total is
// its own error.
func TestCreateOrderSendsTheExpectedTotal(t *testing.T) {
	var got map[string]any
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got = nil
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status == http.StatusConflict {
				_, _ = w.Write(fixture(t, "order_total_changed.json"))
				return
			}
			_, _ = w.Write(fixture(t, "order_by_uuid.json"))
		}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	body := OrderCreate{
		PayWayID:      3,
		ExpectedTotal: ExpectedTotal{Amount: 93626, Currency: "EUR"},
	}

	_, err := c.CreateOrder(t.Context(), "shop.example.test", "el",
		"cart-uuid", body)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"amount": float64(93626), "currency": "EUR",
	}, got["expectedTotal"])

	status = http.StatusConflict
	_, err = c.CreateOrder(t.Context(), "shop.example.test", "el",
		"cart-uuid", body)
	require.ErrorIs(t, err, ErrTotalChanged)
	require.ErrorIs(t, err, ErrConflict)
	apiErr, ok := errors.AsType[*APIError](err)
	require.True(t, ok)
	assert.Equal(t, CodeTotalChanged, apiErr.Code)
}

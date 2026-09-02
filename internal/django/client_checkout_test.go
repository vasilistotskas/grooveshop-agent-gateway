package django

import (
	"context"
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
			require.Equal(t, "/api/v1/cart/reserve-stock", r.URL.Path)
			gotMethod = r.Method
			gotCartID = r.Header.Get("X-Cart-Id")
			gotGateway = r.Header.Get("X-Internal-Gateway")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "reserve_stock.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	res, err := c.ReserveStock(context.Background(),
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
	_, err := c.ReserveStock(context.Background(),
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
	_, err := c.ReserveStock(context.Background(),
		"shop.example.test", "el", "cart-uuid")
	require.Error(t, err)

	var shortfall *StockShortfall
	assert.False(t, errors.As(err, &shortfall))
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
	_, err := c.ReserveStock(context.Background(),
		"shop.example.test", "el", "cart-uuid")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamDown)
	assert.Equal(t, 1, calls)
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

package mcpsrv

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// shippingCart is the cart an agent names.
const shippingCart = "7d0b2f3e-91a4-4c56-8e2f-3b6a1c9d4e70"

// shippingDjango serves the shipping endpoints and the named cart (the
// coupon cart, as shippingCart), and records each call's headers by path
// and the orderValueAmount each options call sends. The options are ACS
// home delivery, a BoxNow locker and an ACS locker over its weight cap;
// as Django does, it flags the BoxNow locker too large only when a cart
// is named. A call carrying a cart token is answered with tokenStatus
// (401: Django refused the token).
func shippingDjango(
	t *testing.T, tokenStatus int,
) (*handlers, map[string][]http.Header, *[]string) {
	t.Helper()
	fixture := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
			"fixtures", "django", name))
		require.NoError(t, err)
		return b
	}
	stations := fixture("acs_stations_nearest.json")
	locker := fixture("boxnow_nearest.json")
	cart := bytes.ReplaceAll(fixture("cart_with_coupon.json"),
		[]byte("29eb4495-e018-45e7-b59c-6646302bd4ef"), []byte(shippingCart))
	seen := map[string][]http.Header{}
	var orderValues []string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen[r.URL.Path] = append(seen[r.URL.Path], r.Header.Clone())
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("X-Cart-Token") != "" &&
				tokenStatus != http.StatusOK {
				w.WriteHeader(tokenStatus)
				_, _ = w.Write([]byte(`{"detail": "refused"}`))
				return
			}
			named := r.Header.Get("X-Cart-Id") != "" ||
				r.Header.Get("X-Cart-Token") != ""
			switch r.URL.Path {
			case cartPath:
				_, _ = w.Write(cart)
			case optionsPath:
				orderValues = append(orderValues,
					r.URL.Query().Get("orderValueAmount"))
				_, _ = w.Write([]byte(`[` +
					shippingRow("acs", "home_delivery", false, false) + `,` +
					shippingRow("boxnow", "pickup_point", false, named) +
					`,` + shippingRow("acs", "pickup_point", true, false) +
					`]`))
			case "/api/v1/shipping/acs/stations/nearest":
				_, _ = w.Write(stations)
			case "/api/v1/shipping/boxnow/lockers/nearest":
				_, _ = w.Write(locker)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dj := django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, nil)
	return &handlers{deps: Deps{Site: testSite, Django: dj}}, seen,
		&orderValues
}

func shippingRow(provider, kind string, overWeight, tooLarge bool) string {
	return `{"providerCode":"` + provider + `","providerName":"` +
		provider + `","kind":"` + kind + `","price":2.5,` +
		`"currency":"EUR","priority":10,"countryCode":"GR",` +
		`"maxWeightGrams":null,` +
		`"exceedsMaxWeight":` + strconv.FormatBool(overWeight) + `,` +
		`"exceedsParcelSize":` + strconv.FormatBool(tooLarge) + `,` +
		`"metadata":{}}`
}

const (
	cartPath    = "/api/v1/cart"
	optionsPath = "/api/v1/shipping/options"
	nearestPath = "/api/v1/shipping/boxnow/lockers/nearest"
)

func offered(out ShippingOptionsOut) []string {
	var keys []string
	for _, o := range out.Options {
		keys = append(keys, o.ProviderCode+":"+o.Kind)
	}
	return keys
}

func storeContext() context.Context {
	return tenant.NewContext(context.Background(), cartTenant())
}

// signedInWith is signedIn with the token kept, to read its refusal.
func signedInWith(t *testing.T) (context.Context, *django.CartToken) {
	t.Helper()
	tok, err := django.ParseCartToken("Zk3n_Q8vX2-rT5yLmW9aB4cD7eF0gH1iJ6k")
	require.NoError(t, err)
	return django.WithCartToken(storeContext(), tok), tok
}

// With the agent's cart named, the options it does not fit are left out:
// the cart goes to Django as X-Cart-Id, which flags the parcel fit. Free
// shipping is judged on the cart's freeThresholdValue (its items after
// the coupon), whatever value the model passed.
func TestShippingOptionsForACartOfferOnlyWhatItFits(t *testing.T) {
	h, seen, orderValues := shippingDjango(t, http.StatusOK)

	_, out, err := h.getShippingOptions(storeContext(), nil,
		ShippingOptionsIn{
			CountryCode: "GR", CartID: shippingCart, OrderValue: "929.36",
		})

	require.NoError(t, err)
	assert.Equal(t, []string{"acs:home_delivery"}, offered(out))
	require.Len(t, seen[optionsPath], 1)
	assert.Equal(t, shippingCart, seen[optionsPath][0].Get("X-Cart-Id"))
	assert.Empty(t, seen[optionsPath][0].Get("X-Cart-Token"))
	assert.Equal(t, []string{"836.42"}, *orderValues)
}

// Without a cart nothing is judged for size, only for the weight given,
// and free shipping on the value the model passed.
func TestShippingOptionsWithoutACartLeaveOutOnlyTheOverweight(t *testing.T) {
	h, seen, orderValues := shippingDjango(t, http.StatusOK)

	_, out, err := h.getShippingOptions(storeContext(), nil,
		ShippingOptionsIn{CountryCode: "GR", OrderValue: "40.00"})

	require.NoError(t, err)
	assert.Equal(t,
		[]string{"acs:home_delivery", "boxnow:pickup_point"}, offered(out))
	require.Len(t, seen[optionsPath], 1)
	assert.Empty(t, seen[optionsPath][0].Get("X-Cart-Id"))
	assert.Empty(t, seen[cartPath], "no cart is read or created")
	assert.Equal(t, []string{"40.00"}, *orderValues)
}

// A signed-in shopper's cart is judged through their cart token, as the
// cart tools reach it, whatever cartId the model names.
func TestShippingOptionsForASignedInShopperUseTheirCartToken(t *testing.T) {
	h, seen, orderValues := shippingDjango(t, http.StatusOK)
	ctx, _ := signedInWith(t)

	_, out, err := h.getShippingOptions(ctx, nil,
		ShippingOptionsIn{CountryCode: "GR", CartID: shippingCart})

	require.NoError(t, err)
	assert.Equal(t, []string{"acs:home_delivery"}, offered(out))
	for _, path := range []string{cartPath, optionsPath} {
		require.Len(t, seen[path], 1, path)
		assert.NotEmpty(t, seen[path][0].Get("X-Cart-Token"), path)
		assert.Empty(t, seen[path][0].Get("X-Cart-Id"), path)
	}
	assert.Equal(t, []string{"836.42"}, *orderValues)
}

// A refused token is reported like the cart tools report it and marked
// on the token, so the turn ends with cartTokenRefused and the widget
// mints a new one; the turn's later calls do not try it again.
func TestShippingOptionsReportARefusedCartToken(t *testing.T) {
	h, seen, _ := shippingDjango(t, http.StatusUnauthorized)
	ctx, tok := signedInWith(t)

	_, _, err := h.getShippingOptions(ctx, nil,
		ShippingOptionsIn{CountryCode: "GR"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cart could not be reached")
	assert.True(t, tok.Refused())

	_, _, err = h.getShippingOptions(ctx, nil,
		ShippingOptionsIn{CountryCode: "GR"})
	require.Error(t, err)
	assert.Len(t, seen[cartPath], 1, "a refused token is not resent")
	assert.Empty(t, seen[optionsPath])
}

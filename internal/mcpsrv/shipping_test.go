package mcpsrv

import (
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

// shippingDjango serves the shipping endpoints and records each call's
// headers by path. The options are ACS home delivery, a BoxNow locker and
// an ACS locker over its weight cap; as Django does, it flags the BoxNow
// locker too large only when a cart is named. A call carrying a cart
// token is answered with tokenStatus (401: Django refused the token).
func shippingDjango(
	t *testing.T, tokenStatus int,
) (*handlers, map[string][]http.Header) {
	t.Helper()
	fixture := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
			"fixtures", "django", name))
		require.NoError(t, err)
		return b
	}
	stations := fixture("acs_stations_nearest.json")
	locker := fixture("boxnow_nearest.json")
	seen := map[string][]http.Header{}
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
			case "/api/v1/shipping/options":
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
	return &handlers{deps: Deps{Site: testSite, Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, nil)}}, seen
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
// the cart goes to Django as X-Cart-Id, which flags the parcel fit.
func TestShippingOptionsForACartOfferOnlyWhatItFits(t *testing.T) {
	h, seen := shippingDjango(t, http.StatusOK)

	_, out, err := h.getShippingOptions(storeContext(), nil,
		ShippingOptionsIn{CountryCode: "GR", CartID: shippingCart})

	require.NoError(t, err)
	assert.Equal(t, []string{"acs:home_delivery"}, offered(out))
	require.Len(t, seen[optionsPath], 1)
	assert.Equal(t, shippingCart, seen[optionsPath][0].Get("X-Cart-Id"))
	assert.Empty(t, seen[optionsPath][0].Get("X-Cart-Token"))
}

// Without a cart nothing is judged for size, only for the weight given.
func TestShippingOptionsWithoutACartLeaveOutOnlyTheOverweight(t *testing.T) {
	h, seen := shippingDjango(t, http.StatusOK)

	_, out, err := h.getShippingOptions(storeContext(), nil,
		ShippingOptionsIn{CountryCode: "GR"})

	require.NoError(t, err)
	assert.Equal(t,
		[]string{"acs:home_delivery", "boxnow:pickup_point"}, offered(out))
	require.Len(t, seen[optionsPath], 1)
	assert.Empty(t, seen[optionsPath][0].Get("X-Cart-Id"))
}

// A signed-in shopper's cart is judged through their cart token, as the
// cart tools reach it, whatever cartId the model names.
func TestShippingOptionsForASignedInShopperUseTheirCartToken(t *testing.T) {
	h, seen := shippingDjango(t, http.StatusOK)
	ctx, _ := signedInWith(t)

	_, out, err := h.getShippingOptions(ctx, nil,
		ShippingOptionsIn{CountryCode: "GR", CartID: shippingCart})

	require.NoError(t, err)
	assert.Equal(t, []string{"acs:home_delivery"}, offered(out))
	require.Len(t, seen[optionsPath], 1)
	assert.NotEmpty(t, seen[optionsPath][0].Get("X-Cart-Token"))
	assert.Empty(t, seen[optionsPath][0].Get("X-Cart-Id"))
}

// A refused token is reported like the cart tools report it and marked
// on the token, so the turn ends with cartTokenRefused and the widget
// mints a new one; the turn's later calls do not try it again.
func TestShippingOptionsReportARefusedCartToken(t *testing.T) {
	h, seen := shippingDjango(t, http.StatusUnauthorized)
	ctx, tok := signedInWith(t)

	_, _, err := h.getShippingOptions(ctx, nil,
		ShippingOptionsIn{CountryCode: "GR"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cart could not be reached")
	assert.True(t, tok.Refused())

	_, _, err = h.getShippingOptions(ctx, nil,
		ShippingOptionsIn{CountryCode: "GR"})
	require.Error(t, err)
	assert.Len(t, seen[optionsPath], 1, "a refused token is not resent")
}

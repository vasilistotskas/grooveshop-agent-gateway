package mcpsrv

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// shippingDjango lists ACS home delivery, a BoxNow locker and an ACS
// locker over its weight cap, and records each options call. As Django
// does, it flags the BoxNow locker too large only when a cart is named.
func shippingDjango(t *testing.T) (*handlers, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/shipping/free-shipping-info" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			seen = append(seen, r)
			assert.Equal(t, "/api/v1/shipping/options", r.URL.Path)
			_, _ = w.Write([]byte(`[` +
				shippingRow("acs", "home_delivery", false, false) + `,` +
				shippingRow("boxnow", "pickup_point", false,
					r.Header.Get("X-Cart-Id") != "") + `,` +
				shippingRow("acs", "pickup_point", true, false) + `]`))
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &handlers{deps: Deps{Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, nil)}}, &seen
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

func offered(out ShippingOptionsOut) []string {
	var keys []string
	for _, o := range out.Options {
		keys = append(keys, o.ProviderCode+":"+o.Kind)
	}
	return keys
}

// With the agent's cart named, the options it does not fit are left out:
// the cart goes to Django as X-Cart-Id, which flags the parcel fit.
func TestShippingOptionsForACartOfferOnlyWhatItFits(t *testing.T) {
	h, seen := shippingDjango(t)
	ctx := tenant.NewContext(context.Background(), cartTenant())

	_, out, err := h.getShippingOptions(ctx, nil,
		ShippingOptionsIn{CountryCode: "GR", CartID: shippingCart})

	require.NoError(t, err)
	assert.Equal(t, []string{"acs:home_delivery"}, offered(out))
	require.Len(t, *seen, 1)
	assert.Equal(t, shippingCart, (*seen)[0].Header.Get("X-Cart-Id"))
}

// Without a cart nothing is judged for size, only for the weight given.
func TestShippingOptionsWithoutACartLeaveOutOnlyTheOverweight(t *testing.T) {
	h, seen := shippingDjango(t)
	ctx := tenant.NewContext(context.Background(), cartTenant())

	_, out, err := h.getShippingOptions(ctx, nil,
		ShippingOptionsIn{CountryCode: "GR"})

	require.NoError(t, err)
	assert.Equal(t,
		[]string{"acs:home_delivery", "boxnow:pickup_point"}, offered(out))
	require.Len(t, *seen, 1)
	assert.Empty(t, (*seen)[0].Header.Get("X-Cart-Id"))
}

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

// shippingDjango lists ACS home delivery, a BoxNow locker the cart is too
// large for and an ACS locker over its weight cap, and records each
// options call.
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
				shippingRow("boxnow", "pickup_point", false, true) + `,` +
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

// An agent is offered only the options the cart fits; the tool names no
// cart, so it never sends one.
func TestShippingOptionsOfferOnlyWhatTheCartFits(t *testing.T) {
	h, seen := shippingDjango(t)
	ctx := tenant.NewContext(context.Background(), cartTenant())

	_, out, err := h.getShippingOptions(ctx, nil,
		ShippingOptionsIn{CountryCode: "GR"})

	require.NoError(t, err)
	require.Len(t, out.Options, 1)
	assert.Equal(t, "acs", out.Options[0].ProviderCode)
	assert.Equal(t, "home_delivery", out.Options[0].Kind)
	require.Len(t, *seen, 1)
	assert.Empty(t, (*seen)[0].Header.Get("X-Cart-Id"))
}

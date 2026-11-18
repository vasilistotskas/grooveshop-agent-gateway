package acp

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django/djangotest"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// quotedDjango serves the coupon cart (929.36 of items, 836.42 after its
// discount), quotes ACS home delivery at 3.50 and pay way 7 at 2.00, as
// Django prices them.
func quotedDjango(t *testing.T) *django.Client {
	t.Helper()
	cart := readFixture(t, "cart_with_coupon.json")
	payWay := []byte(`{"id":7,"key":"PAY_ON_DELIVERY","translations":{},` +
		`"active":true,"cost":2.00,"freeThreshold":0,"sortOrder":1,` +
		`"providerCode":"cash_on_delivery","settlement":"courier_cash"}`)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cart)
		})
	mux.HandleFunc("GET /api/v1/pay_way/7",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(payWay)
		})
	mux.HandleFunc("POST /api/v1/cart/quote", djangotest.Store{
		Cart: cart,
		ShippingOptions: []byte(`[{"providerCode":"acs",` +
			`"providerName":"ACS Courier","kind":"home_delivery",` +
			`"price":3.50,"exceedsMaxWeight":false,` +
			`"exceedsParcelSize":false}]`),
		PayWays: map[int64][]byte{7: payWay},
	}.Quote)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	log := slog.New(slog.DiscardHandler)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
}

// The offered option and every total row are the store's quote, and the
// total is the one the order is placed at, which the render records on
// the session for its order.
func TestRenderShowsTheQuote(t *testing.T) {
	tn := testTenant()
	s := checkout.NewSession("public", tn.Domain, "acp", renderSessionCart)
	s.Fulfillment = checkout.Fulfillment{
		Kind:         checkout.FulfillmentHomeDelivery,
		ProviderCode: "acs",
		CountryCode:  "GR", City: "Αθήνα", Zipcode: "10431",
		Street: "Πανεπιστημίου", StreetNumber: "12",
	}
	s.PayWayID = 7
	s.Recompute()

	payload, err := Render(t.Context(), quotedDjango(t), tn,
		testLinks(tn), s)
	require.NoError(t, err)

	require.Len(t, payload.FulfillmentOptions, 1)
	assert.Equal(t, []Total{{
		Type: "total", DisplayText: "Shipping", Amount: 350,
	}}, payload.FulfillmentOptions[0].Totals)
	totals := map[string]int64{}
	for _, total := range payload.Totals {
		totals[total.Type] = total.Amount
	}
	want := int64(92936 - 9294 + 350 + 200)
	assert.Equal(t, map[string]int64{
		"subtotal":    92936,
		"discount":    9294,
		"fulfillment": 350,
		"fee":         200,
		"total":       want,
	}, totals)
	assert.Equal(t, &django.ExpectedTotal{Amount: want, Currency: "EUR"},
		s.ShownTotal, "the total its order is placed at")
}

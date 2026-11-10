package acp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// thresholdDjango serves the coupon cart (929.36 of items, 836.42 after
// its discount) and prices delivery as Django does with a 900.00
// free-shipping threshold, on the orderValueAmount it is sent; pay way 7
// costs 2.00, waived from 900.00.
func thresholdDjango(t *testing.T) *django.Client {
	t.Helper()
	cart, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "django", "cart_with_coupon.json"))
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cart)
		})
	mux.HandleFunc("GET /api/v1/shipping/options",
		func(w http.ResponseWriter, r *http.Request) {
			value, err := money.MinorUnits(
				r.URL.Query().Get("orderValueAmount"))
			assert.NoError(t, err)
			price := "3.50"
			if value >= 90000 {
				price = "0.0"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"providerCode":"acs",` +
				`"providerName":"ACS Courier","kind":"home_delivery",` +
				`"price":` + price + `,"currency":"EUR","priority":10,` +
				`"countryCode":"GR","maxWeightGrams":null,` +
				`"exceedsMaxWeight":false,"exceedsParcelSize":false,` +
				`"metadata":{}}]`))
		})
	mux.HandleFunc("GET /api/v1/pay_way/7",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":7,"key":"PAY_ON_DELIVERY",` +
				`"translations":{},"active":true,"cost":2.00,` +
				`"freeThreshold":900.00,"sortOrder":1,` +
				`"providerCode":"cash_on_delivery",` +
				`"settlement":"courier_cash"}`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
}

// A coupon that takes the cart below both free thresholds is charged
// delivery and the pay way's fee, in the offered option and in the
// totals, as Django charges the order.
func TestRenderJudgesFreeThresholdsAfterDiscounts(t *testing.T) {
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

	payload, err := Render(context.Background(), thresholdDjango(t), tn,
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
	assert.Equal(t, map[string]int64{
		"subtotal":    92936,
		"discount":    9294,
		"fulfillment": 350,
		"fee":         200,
		"total":       92936 - 9294 + 350 + 200,
	}, totals)
}

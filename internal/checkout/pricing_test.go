package checkout

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func pricingTenant() *tenant.Tenant {
	return &tenant.Tenant{
		SchemaName:      "public",
		DefaultLocale:   "el",
		DefaultCurrency: "EUR",
		PrimaryDomain:   "shop.example.test",
		Domain:          "shop.example.test",
		// A store that takes discount codes.
		Features: django.StoreFeatures{Promotions: true},
	}
}

// pricingCart renders a one-line cart (2 × 100.00 EUR) with the given
// promotion state, in the upstream wire shape. Its freeThresholdValue is
// Django's: the items after the promotion discount, never below zero.
func pricingCart(promo string, freeShipping bool, codes []string) string {
	rawCodes, _ := json.Marshal(codes)
	if codes == nil {
		rawCodes = []byte("[]")
	}
	discount, err := money.MinorUnits(promo)
	if err != nil {
		panic(err)
	}
	threshold := money.Format(max(20000-discount, 0))
	return `{"id":1,"uuid":"29eb4495-e018-45e7-b59c-6646302bd4ef",` +
		`"items":[{"id":10,"product":{"id":5,"translations":` +
		`{"el":{"name":"Δοκιμαστικό","description":""}},` +
		`"finalPrice":100.00},"quantity":2,"finalPrice":100.00,` +
		`"totalPrice":200.00}],"totalPrice":200.00,` +
		`"totalDiscountValue":12.00,"promotionDiscount":` + promo + `,` +
		`"freeThresholdValue":` + threshold + `,` +
		`"promotionFreeShipping":` + strconv.FormatBool(freeShipping) + `,` +
		`"appliedCouponCodes":` + string(rawCodes) + `,` +
		`"totalItems":2,"totalItemsUnique":1,"totalWeightGrams":107,` +
		`"currency":"EUR"}`
}

// pricingSessionCart is the cart every pricing test session names.
const pricingSessionCart = "29eb4495-e018-45e7-b59c-6646302bd4ef"

// pricingDjango serves the given cart and one acs home_delivery option at
// the given price, flagged over its weight cap when overWeight is set and
// too large for its parcels when tooLarge is. The options handler fails
// the test unless the cart weight and the session's cart are forwarded.
func pricingDjango(
	t *testing.T, cart string, shippingPrice string,
	overWeight, tooLarge bool,
) *django.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(cart))
		})
	mux.HandleFunc("GET /api/v1/shipping/options",
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "107", r.URL.Query().Get("weightGrams"),
				"cart weight forwarded to the rate's weight cap")
			assert.Equal(t, pricingSessionCart, r.Header.Get("X-Cart-Id"),
				"cart named so Django checks it against the parcels")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"providerCode":"acs",` +
				`"providerName":"ACS Courier","kind":"home_delivery",` +
				`"price":` + shippingPrice + `,"currency":"EUR",` +
				`"priority":10,"countryCode":"GR","maxWeightGrams":100,` +
				`"exceedsMaxWeight":` + strconv.FormatBool(overWeight) + `,` +
				`"exceedsParcelSize":` + strconv.FormatBool(tooLarge) + `,` +
				`"metadata":{}}]`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
}

func TestComputePricingDiscounts(t *testing.T) {
	tn := pricingTenant()

	cases := []struct {
		name          string
		cart          string
		shippingPrice string
		withDelivery  bool
		overWeight    bool
		tooLarge      bool

		wantSubtotal int64
		wantDiscount int64
		wantMarkdown int64
		wantFreeShip bool
		wantFee      int64
		wantHasDel   bool
		wantTotal    int64
	}{
		{
			name:         "no promotion",
			cart:         pricingCart("0.0", false, nil),
			wantSubtotal: 20000,
			wantMarkdown: 1200,
			wantTotal:    20000,
		},
		{
			name:         "promotion discount reduces total",
			cart:         pricingCart("20.00", false, []string{"SAVE20"}),
			wantSubtotal: 20000,
			wantDiscount: 2000,
			wantMarkdown: 1200,
			wantTotal:    18000,
		},
		{
			name:          "delivery fee applies on top of the discount",
			cart:          pricingCart("20.00", false, []string{"SAVE20"}),
			shippingPrice: "3.50",
			withDelivery:  true,
			wantSubtotal:  20000,
			wantDiscount:  2000,
			wantMarkdown:  1200,
			wantHasDel:    true,
			wantFee:       350,
			wantTotal:     18350,
		},
		{
			name:          "promotional free shipping zeroes the delivery fee",
			cart:          pricingCart("20.00", true, []string{"SHIPFREE"}),
			shippingPrice: "3.50",
			withDelivery:  true,
			wantSubtotal:  20000,
			wantDiscount:  2000,
			wantMarkdown:  1200,
			wantFreeShip:  true,
			wantHasDel:    true,
			wantFee:       0,
			wantTotal:     18000,
		},
		{
			name:          "an option over its weight cap prices no delivery",
			cart:          pricingCart("20.00", false, []string{"SAVE20"}),
			shippingPrice: "3.50",
			withDelivery:  true,
			overWeight:    true,
			wantSubtotal:  20000,
			wantDiscount:  2000,
			wantMarkdown:  1200,
			wantTotal:     18000,
		},
		{
			name:          "an option too large for the cart prices no delivery",
			cart:          pricingCart("20.00", false, []string{"SAVE20"}),
			shippingPrice: "3.50",
			withDelivery:  true,
			tooLarge:      true,
			wantSubtotal:  20000,
			wantDiscount:  2000,
			wantMarkdown:  1200,
			wantTotal:     18000,
		},
		{
			name:         "discount above subtotal clamps total at zero",
			cart:         pricingCart("250.00", false, []string{"MEGA"}),
			wantSubtotal: 20000,
			wantDiscount: 25000,
			wantMarkdown: 1200,
			wantTotal:    0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shipping := tc.shippingPrice
			if shipping == "" {
				shipping = "0.0"
			}
			dj := pricingDjango(t, tc.cart, shipping,
				tc.overWeight, tc.tooLarge)

			s := NewSession("public", tn.Domain, "acp", pricingSessionCart)
			if tc.withDelivery {
				s.Fulfillment = Fulfillment{
					Kind:         FulfillmentHomeDelivery,
					ProviderCode: "acs",
					CountryCode:  "GR",
				}
			}

			p, cart, err := ComputePricing(context.Background(), dj, tn, s)
			require.NoError(t, err)
			require.NotNil(t, cart)

			assert.Equal(t, tc.wantSubtotal, p.ItemsSubtotal, "subtotal")
			assert.Equal(t, tc.wantDiscount, p.DiscountTotal, "discount")
			assert.Equal(t, tc.wantMarkdown, p.MarkdownTotal, "markdown")
			assert.Equal(t, tc.wantFreeShip, p.FreeShipping, "free shipping")
			assert.Equal(t, tc.wantHasDel, p.HasDelivery, "has delivery")
			assert.Equal(t, tc.wantFee, p.DeliveryFee, "delivery fee")
			assert.Equal(t, tc.wantTotal, p.Total, "total")
		})
	}
}

// thresholdDjango prices delivery as Django does with a 190.00 free-
// shipping threshold, on the orderValueAmount it is sent, and serves pay
// way 7: a 2.00 fee waived from 190.00.
func thresholdDjango(t *testing.T, cart string) *django.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(cart))
		})
	mux.HandleFunc("GET /api/v1/shipping/options",
		func(w http.ResponseWriter, r *http.Request) {
			value, err := money.MinorUnits(
				r.URL.Query().Get("orderValueAmount"))
			assert.NoError(t, err)
			price := "3.50"
			if value >= 19000 {
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
				`"freeThreshold":190.00,"sortOrder":1,` +
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

// Django judges free shipping and the pay way's free threshold on the
// items after discounts (freeThresholdValue), so a discount that takes a
// 200.00 cart below 190.00 is charged both, as the order will be.
func TestComputePricingJudgesFreeThresholdsAfterDiscounts(t *testing.T) {
	cases := []struct {
		name        string
		cart        string
		wantFee     int64
		wantPayment int64
		wantHasPay  bool
		wantTotal   int64
	}{
		{
			name:      "items above both thresholds pay neither",
			cart:      pricingCart("0.0", false, nil),
			wantTotal: 20000,
		},
		{
			name:        "a discount below both thresholds pays both",
			cart:        pricingCart("20.00", false, []string{"SAVE20"}),
			wantFee:     350,
			wantPayment: 200,
			wantHasPay:  true,
			wantTotal:   18000 + 350 + 200,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tn := pricingTenant()
			s := NewSession("public", tn.Domain, "ucp", pricingSessionCart)
			s.Fulfillment = Fulfillment{
				Kind:         FulfillmentHomeDelivery,
				ProviderCode: "acs",
				CountryCode:  "GR",
			}
			s.PayWayID = 7

			p, _, err := ComputePricing(context.Background(),
				thresholdDjango(t, tc.cart), tn, s)
			require.NoError(t, err)

			assert.True(t, p.HasDelivery, "has delivery")
			assert.Equal(t, tc.wantFee, p.DeliveryFee, "delivery fee")
			assert.Equal(t, tc.wantHasPay, p.HasPaymentFee,
				"has payment fee")
			assert.Equal(t, tc.wantPayment, p.PaymentFee, "payment fee")
			assert.Equal(t, tc.wantTotal, p.Total, "total")
		})
	}
}

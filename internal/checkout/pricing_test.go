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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func pricingTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName:      "public",
			DefaultLocale:   "el",
			DefaultCurrency: "EUR",
			PrimaryDomain:   "shop.example.test",
		},
		Domain: "shop.example.test",
	}
}

// pricingCart renders a one-line cart (2 × 100.00 EUR) with the given
// promotion state, in the upstream wire shape.
func pricingCart(promo string, freeShipping bool, codes []string) string {
	rawCodes, _ := json.Marshal(codes)
	if codes == nil {
		rawCodes = []byte("[]")
	}
	return `{"id":1,"uuid":"29eb4495-e018-45e7-b59c-6646302bd4ef",` +
		`"items":[{"id":10,"product":{"id":5,"translations":` +
		`{"el":{"name":"Δοκιμαστικό","description":""}},` +
		`"finalPrice":100.00},"quantity":2,"finalPrice":100.00,` +
		`"totalPrice":200.00}],"totalPrice":200.00,` +
		`"totalDiscountValue":12.00,"promotionDiscount":` + promo + `,` +
		`"promotionFreeShipping":` + strconv.FormatBool(freeShipping) + `,` +
		`"appliedCouponCodes":` + string(rawCodes) + `,` +
		`"totalItems":2,"totalItemsUnique":1,"currency":"EUR"}`
}

// pricingDjango serves the given cart and one acs home_delivery option at
// the given price.
func pricingDjango(
	t *testing.T, cart string, shippingPrice string,
) *django.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(cart))
		})
	mux.HandleFunc("GET /api/v1/shipping/options",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"providerCode":"acs",` +
				`"providerName":"ACS Courier","kind":"home_delivery",` +
				`"price":` + shippingPrice + `,"currency":"EUR",` +
				`"priority":10,"metadata":{}}]`))
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
			dj := pricingDjango(t, tc.cart, shipping)

			s := NewSession("public", tn.Domain, "acp",
				"29eb4495-e018-45e7-b59c-6646302bd4ef")
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

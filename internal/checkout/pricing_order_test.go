package checkout

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

const placedOrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"

// A placed order prices from the order. Django deletes the cart with it,
// so the cart must not even be asked for: it would answer a new one, and
// the checkout that placed the order would fail to render.
func TestComputePricingPlacedOrder(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart",
		func(http.ResponseWriter, *http.Request) {
			t.Error("a placed order must not read the cart")
		})
	mux.HandleFunc("GET /api/v1/order/uuid/"+placedOrderUUID,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id": 684, "uuid": "` + placedOrderUUID + `",
				"items": [{
					"product": {"id": 19, "mainImagePath": "p.jpg",
						"translations": {"el": {"name": "Powerbank"}}},
					"quantity": 2, "price": 39.58, "totalPrice": 79.16
				}],
				"pricingBreakdown": {
					"itemsSubtotal": 79.16, "shippingCost": 3,
					"paymentMethodFee": 2, "discount": 5,
					"loyaltyDiscount": 1, "giftCardAmount": 0.5,
					"grandTotal": 77.66, "currency": "EUR"
				}
			}`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dj := django.New(srv.URL+"/api/v1", "platform.test", "secret",
		time.Second, slog.New(slog.DiscardHandler), obs.NewMetrics())
	tn := &tenant.Tenant{
		TenantConfig: django.TenantConfig{DefaultLocale: "el"},
		Domain:       "demostore.test",
	}

	p, cart, err := ComputePricing(context.Background(), dj, tn,
		&Session{CartID: "gone", OrderUUID: placedOrderUUID})
	require.NoError(t, err)

	assert.Nil(t, cart)
	assert.Equal(t, []PricedLine{{
		LineID: 19, ProductID: 19, Title: "Powerbank", ImagePath: "p.jpg",
		Quantity: 2, UnitMinor: 3958, TotalMinor: 7916,
	}}, p.Lines)
	assert.EqualValues(t, 7916, p.ItemsSubtotal)
	assert.True(t, p.HasDelivery)
	assert.EqualValues(t, 300, p.DeliveryFee)
	assert.True(t, p.HasPaymentFee)
	assert.EqualValues(t, 200, p.PaymentFee)
	assert.EqualValues(t, 650, p.DiscountTotal, "every reduction counts")
	assert.EqualValues(t, 7766, p.Total)
	assert.Equal(t, p.Total,
		p.ItemsSubtotal+p.DeliveryFee+p.PaymentFee-p.DiscountTotal,
		"the rows a platform shows add up to the total")
}

// A failed read must surface: the renderers report it, and a retry of the
// completed checkout renders once Django answers.
func TestComputePricingPlacedOrderUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
	t.Cleanup(srv.Close)
	dj := django.New(srv.URL+"/api/v1", "platform.test", "secret",
		time.Second, slog.New(slog.DiscardHandler), obs.NewMetrics())

	_, _, err := ComputePricing(context.Background(), dj,
		&tenant.Tenant{Domain: "demostore.test"},
		&Session{OrderUUID: placedOrderUUID})
	assert.ErrorIs(t, err, django.ErrUpstreamDown)
}

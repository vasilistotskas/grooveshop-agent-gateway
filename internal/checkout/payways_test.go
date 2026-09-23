package checkout

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

const (
	payWayCOD = `{"id":1,"active":true,"providerCode":"cash_on_delivery",` +
		`"settlement":"courier_cash","cost":2.00,"freeThreshold":0.0}`
	payWayViva = `{"id":2,"active":true,"providerCode":"viva_wallet",` +
		`"settlement":"online","cost":0.0,"freeThreshold":0.0}`
)

// payWayDjango serves the pay-way list and detail endpoints plus the
// pricing cart, recording the list's query so tests can assert the
// carrier filter.
func payWayDjango(t *testing.T) (*django.Client, func() url.Values) {
	t.Helper()
	var (
		mu    sync.Mutex
		query url.Values
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pay_way",
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			query = r.URL.Query()
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":2,"results":[` + payWayCOD +
				`,` + payWayViva + `]}`))
		})
	mux.HandleFunc("GET /api/v1/pay_way/1",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(payWayCOD))
		})
	mux.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(pricingCart("0.0", false, nil)))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	dj := django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
	return dj, func() url.Values {
		mu.Lock()
		defer mu.Unlock()
		return query
	}
}

func lockerDelivery() Fulfillment {
	return Fulfillment{
		Kind: FulfillmentPickupPoint, ProviderCode: django.ShippingProviderBoxNow,
		CountryCode: "GR", City: "Αθήνα", Zipcode: "10431", Street: "Ερμού",
		BoxnowLockerID: "4",
	}
}

// A method valid for one carrier can be one another carrier cannot
// settle, so a complete delivery scopes the list; an incomplete one
// cannot, and completion re-checks.
func TestPayWaysForScopesByCompleteDelivery(t *testing.T) {
	dj, lastQuery := payWayDjango(t)
	tn := pricingTenant()

	_, err := PayWaysFor(context.Background(), dj, tn, lockerDelivery())
	require.NoError(t, err)
	assert.Equal(t, "boxnow", lastQuery().Get("shippingProviderCode"))
	assert.Equal(t, FulfillmentPickupPoint, lastQuery().Get("shippingKind"))
	assert.Equal(t, "100", lastQuery().Get("pageSize"),
		"the default page of 12 would hide methods")

	_, err = PayWaysFor(context.Background(), dj, tn,
		Fulfillment{Kind: FulfillmentPickupPoint})
	require.NoError(t, err)
	assert.Empty(t, lastQuery().Get("shippingProviderCode"))
}

func TestOfflinePayWayPicksCollectLater(t *testing.T) {
	dj, _ := payWayDjango(t)
	pw, err := OfflinePayWay(context.Background(), dj, pricingTenant(),
		lockerDelivery())
	require.NoError(t, err)
	require.NotNil(t, pw)
	assert.EqualValues(t, 1, pw.ID)
}

func TestAvailablePayWayRefusesAMethodNotOffered(t *testing.T) {
	dj, _ := payWayDjango(t)
	s := &Session{Fulfillment: lockerDelivery(), PayWayID: 99}
	_, err := availablePayWay(context.Background(), dj, pricingTenant(), s)
	assert.ErrorIs(t, err, ErrPayWayUnavailable)

	s.PayWayID = 2
	pw, err := availablePayWay(context.Background(), dj, pricingTenant(), s)
	require.NoError(t, err)
	assert.Equal(t, django.ProviderVivaWallet, pw.ProviderCode)
}

// A selected method's fee is part of the total the buyer approves, read
// from the pay-way detail endpoint.
func TestComputePricingAddsTheSelectedPayWayFee(t *testing.T) {
	dj, _ := payWayDjango(t)
	s := &Session{CartID: "29eb4495-e018-45e7-b59c-6646302bd4ef", PayWayID: 1}
	p, _, err := ComputePricing(context.Background(), dj, pricingTenant(), s)
	require.NoError(t, err)
	assert.True(t, p.HasPaymentFee)
	assert.EqualValues(t, 200, p.PaymentFee)
	assert.EqualValues(t, 20200, p.Total)
}

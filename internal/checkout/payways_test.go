package checkout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	log := slog.New(slog.DiscardHandler)
	dj := django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
	return dj, func() url.Values {
		mu.Lock()
		defer mu.Unlock()
		return query
	}
}

func acsDelivery() Fulfillment {
	return Fulfillment{
		Kind: FulfillmentHomeDelivery, ProviderCode: django.ShippingProviderACS,
		CountryCode: "GR", City: "Αθήνα", Zipcode: "10431", Street: "Ερμού 5",
	}
}

// A method valid for one carrier can be one another carrier cannot
// settle, so a complete delivery scopes the list; an incomplete one
// cannot, and completion re-checks.
func TestPayWaysForScopesByCompleteDelivery(t *testing.T) {
	dj, lastQuery := payWayDjango(t)
	tn := pricingTenant()

	_, err := PayWaysFor(t.Context(), dj, tn, acsDelivery())
	require.NoError(t, err)
	assert.Equal(t, "acs", lastQuery().Get("shippingProviderCode"))
	assert.Equal(t, FulfillmentHomeDelivery, lastQuery().Get("shippingKind"))
	assert.Equal(t, "100", lastQuery().Get("pageSize"),
		"the default page of 12 would hide methods")

	_, err = PayWaysFor(t.Context(), dj, tn,
		Fulfillment{Kind: FulfillmentHomeDelivery})
	require.NoError(t, err)
	assert.Empty(t, lastQuery().Get("shippingProviderCode"))
}

func TestOfflinePayWayPicksCollectLater(t *testing.T) {
	dj, _ := payWayDjango(t)
	pw, err := OfflinePayWay(t.Context(), dj, pricingTenant(),
		acsDelivery())
	require.NoError(t, err)
	require.NotNil(t, pw)
	assert.EqualValues(t, 1, pw.ID)
}

func TestAvailablePayWayRefusesAMethodNotOffered(t *testing.T) {
	dj, _ := payWayDjango(t)
	s := &Session{Fulfillment: acsDelivery(), PayWayID: 99}
	_, err := availablePayWay(t.Context(), dj, pricingTenant(), s)
	require.ErrorIs(t, err, ErrPayWayUnavailable)

	s.PayWayID = 2
	pw, err := availablePayWay(t.Context(), dj, pricingTenant(), s)
	require.NoError(t, err)
	assert.Equal(t, django.ProviderVivaWallet, pw.ProviderCode)
}

// Only a definitive 4xx proves nothing was created; anything else leaves
// the order's existence unknown.
func TestRefusedIsOnlyADefinitiveRejection(t *testing.T) {
	for _, err := range []error{
		django.ErrValidation, django.ErrConflict, django.ErrNotFound,
		django.ErrThrottled, django.ErrUnauthorized, django.ErrForbidden,
	} {
		assert.True(t, refused(fmt.Errorf("create: %w", err)), err.Error())
	}
	for _, err := range []error{
		django.ErrUpstreamDown, context.DeadlineExceeded,
		errors.New("decode: unexpected EOF"),
	} {
		assert.False(t, refused(err), err.Error())
	}
}

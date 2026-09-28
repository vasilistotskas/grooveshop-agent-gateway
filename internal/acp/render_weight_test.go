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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// overCapDjango serves the usual cart and pay ways, and one ACS
// home-delivery option that the cart has outgrown.
func overCapDjango(t *testing.T) *django.Client {
	t.Helper()
	fixture := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
				"fixtures", "django", name))
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart", fixture("cart_with_items.json"))
	mux.HandleFunc("GET /api/v1/pay_way", fixture("pay_way.json"))
	mux.HandleFunc("GET /api/v1/pay_way/1", fixture("pay_way_1.json"))
	mux.HandleFunc("GET /api/v1/shipping/options",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"providerCode":"acs",` +
				`"providerName":"ACS Courier","kind":"home_delivery",` +
				`"price":3.5,"currency":"EUR","priority":10,` +
				`"countryCode":"GR","maxWeightGrams":100,` +
				`"exceedsMaxWeight":true,"metadata":{}}]`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
}

func TestAnOverCapSelectionIsNeitherOfferedNorEchoed(t *testing.T) {
	tn := testTenant()
	s := checkout.NewSession("public", tn.Domain, "acp",
		"29eb4495-e018-45e7-b59c-6646302bd4ef")
	s.Fulfillment = checkout.Fulfillment{
		Kind:         checkout.FulfillmentHomeDelivery,
		ProviderCode: "acs",
		CountryCode:  "GR", City: "Αθήνα", Zipcode: "10431",
		Street: "Πανεπιστημίου", StreetNumber: "12",
	}
	s.Recompute()

	payload, err := Render(context.Background(), overCapDjango(t), tn, s)
	require.NoError(t, err)

	assert.Empty(t, payload.FulfillmentOptions,
		"an option over its weight cap cannot be chosen")
	assert.Empty(t, payload.SelectedFulfillmentOptions,
		"a selection the buyer cannot choose again must not be echoed")
}

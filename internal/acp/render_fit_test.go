package acp

import (
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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django/djangotest"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

const renderSessionCart = "29eb4495-e018-45e7-b59c-6646302bd4ef"

// unfitDjango serves the usual cart and pay ways, and quotes one ACS
// home-delivery option that the cart has outgrown: flags are the option's
// exceedsMaxWeight and exceedsParcelSize members. The quote handler
// fails the test unless the session's cart is named.
func unfitDjango(t *testing.T, flags string) *django.Client {
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
	quote := djangotest.Store{
		Cart: readFixture(t, "cart_with_items.json"),
		ShippingOptions: []byte(`[{"providerCode":"acs",` +
			`"providerName":"ACS Courier","kind":"home_delivery",` +
			`"price":3.5,"currency":"EUR","priority":10,` +
			`"countryCode":"GR","maxWeightGrams":100,` +
			flags + `,"metadata":{}}]`),
		PayWays: map[int64][]byte{1: readFixture(t, "pay_way_1.json")},
	}
	mux.HandleFunc("POST /api/v1/cart/quote",
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, renderSessionCart, r.Header.Get("X-Cart-Id"))
			quote.Quote(w, r)
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	log := slog.New(slog.DiscardHandler)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
}

func TestAnUnfitSelectionIsNeitherOfferedNorEchoed(t *testing.T) {
	for name, flags := range map[string]string{
		"over its weight cap": `"exceedsMaxWeight":true,` +
			`"exceedsParcelSize":false`,
		"too large for its parcels": `"exceedsMaxWeight":false,` +
			`"exceedsParcelSize":true`,
	} {
		t.Run(name, func(t *testing.T) {
			tn := testTenant()
			s := checkout.NewSession("public", tn.Domain, "acp",
				renderSessionCart)
			s.Fulfillment = checkout.Fulfillment{
				Kind:         checkout.FulfillmentHomeDelivery,
				ProviderCode: "acs",
				CountryCode:  "GR", City: "Αθήνα", Zipcode: "10431",
				Street: "Πανεπιστημίου 12",
			}
			s.PayWayID = 1
			s.Recompute()

			payload, err := Render(t.Context(),
				unfitDjango(t, flags), tn, testLinks(tn), s)
			require.NoError(t, err)

			assert.Empty(t, payload.FulfillmentOptions,
				"an option the cart does not fit cannot be chosen")
			assert.Empty(t, payload.SelectedFulfillmentOptions,
				"a selection the buyer cannot choose again must not be "+
					"echoed")
			assert.Equal(t, "not_ready_for_payment", payload.Status)
			assert.Contains(t, payload.Messages, Message{
				Type: "error", Code: "invalid",
				Param:      "$.selected_fulfillment_options",
				Resolution: "requires_buyer_input", ContentType: "plain",
				Content: payload.Messages[len(payload.Messages)-1].Content,
			}, "the refused delivery is named, and the buyer chooses again")
			for _, total := range payload.Totals {
				assert.NotEqual(t, "fulfillment", total.Type,
					"a refused delivery has no row in the total")
			}
		})
	}
}

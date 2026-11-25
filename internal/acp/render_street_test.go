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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// line_one is the one street line, which Django splits, and line_two
// what it does not hold: both are kept as sent and rendered back.
func TestAddressLinesAreKeptAsSent(t *testing.T) {
	s := checkout.NewSession("public", testTenant().Domain, "acp",
		"29eb4495-e018-45e7-b59c-6646302bd4ef")
	applyFulfillment(s, &FulfillmentDetails{Address: &Address{
		LineOne: " Πανεπιστημίου 12 ", LineTwo: "3ος όροφος",
		City: "Αθήνα", PostalCode: "10431", Country: "GR",
	}}, []SelectedFulfillmentOption{{OptionID: "acs:home_delivery"}})

	assert.Equal(t, "Πανεπιστημίου 12", s.Fulfillment.Street)
	assert.Equal(t, "3ος όροφος", s.Fulfillment.AddressLine2)
	s.Buyer = checkout.Buyer{
		FirstName: "Μαρία", LastName: "Παπαδοπούλου",
		Email: "maria@example.test", Phone: "+306912345678",
	}
	s.Recompute()
	assert.Equal(t, checkout.StatusReadyForComplete, s.Status,
		"a line without a number is Django's to refuse, not a guess here")

	payload, err := Render(t.Context(), fixtureDjango(t),
		testTenant(), testLinks(testTenant()), s)
	require.NoError(t, err)
	require.NotNil(t, payload.FulfillmentDetails)
	assert.Equal(t, "Πανεπιστημίου 12",
		payload.FulfillmentDetails.Address.LineOne)
	assert.Equal(t, "3ος όροφος", payload.FulfillmentDetails.Address.LineTwo)
}

// A street line Django refused at completion keeps the session from
// being ready, and its read says which field to fix, until the address
// changes.
func TestARefusedStreetLineIsNamedUntilItChanges(t *testing.T) {
	s := checkout.NewSession("public", testTenant().Domain, "acp",
		"29eb4495-e018-45e7-b59c-6646302bd4ef")
	address := &FulfillmentDetails{Address: &Address{
		LineOne: "Πανεπιστημίου", City: "Αθήνα",
		PostalCode: "10431", Country: "GR",
	}}
	applyFulfillment(s, address,
		[]SelectedFulfillmentOption{{OptionID: "acs:home_delivery"}})
	s.Buyer = checkout.Buyer{
		FirstName: "Μαρία", LastName: "Παπαδοπούλου",
		Email: "maria@example.test", Phone: "+306912345678",
	}
	s.DestinationRefused = &checkout.Refusal{
		Code: django.CodeStreetNumberMissing, Detail: "Add the number."}
	s.Recompute()
	require.Equal(t, checkout.StatusIncomplete, s.Status)

	payload, err := Render(t.Context(), fixtureDjango(t),
		testTenant(), testLinks(testTenant()), s)
	require.NoError(t, err)
	var params []string
	for _, m := range payload.Messages {
		if m.Type == "error" {
			params = append(params, m.Code+" "+m.Param)
		}
	}
	assert.Equal(t,
		[]string{"invalid $.fulfillment_details.address.line_one"}, params)

	applyFulfillment(s, address, nil)
	assert.NotNil(t, s.DestinationRefused, "the same address")
	address.Address.LineOne = "Πανεπιστημίου 12"
	applyFulfillment(s, address, nil)
	assert.Nil(t, s.DestinationRefused)
}

// A delivery no carrier makes to the region is refused at state: with no
// option left, another option is no remedy.
func TestARegionNoCarrierDeliversToIsAtState(t *testing.T) {
	refused := &checkout.Refusal{
		Code: django.CodeShippingRegionUnavailable, Detail: "Not there."}
	for name, tc := range map[string]struct {
		options []django.QuoteShippingOption
		param   string
	}{
		"none left": {nil, paramState},
		"another carrier": {
			[]django.QuoteShippingOption{{Kind: "home_delivery"}},
			"$.selected_fulfillment_options",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := refusedChoices(&checkout.Pricing{
				DeliveryRefused: refused, ShippingOptions: tc.options})
			require.Len(t, got, 1)
			assert.Equal(t, tc.param, got[0].Param)
			assert.Contains(t, got[0].Content, "Not there.")
		})
	}
}

// An update that only selects a carrier judges the held region again, so
// one the store stopped serving is asked for again; one that touches no
// delivery reads no regions.
func TestSelectingACarrierJudgesTheRegionAgain(t *testing.T) {
	var asked int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/region/GR/get_regions_by_country_alpha_2",
		func(w http.ResponseWriter, _ *http.Request) {
			asked++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"alpha":"GR-14",` +
				`"translations":{"el":{"name":"Αττική"}}}]`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &Handler{dj: django.New(srv.URL+"/api/v1", "platform.test", "secret",
		time.Second, slog.New(slog.DiscardHandler), obs.NewMetrics())}
	s := checkout.NewSession("public", testTenant().Domain, "acp", "c")
	s.Deliver(checkout.Fulfillment{CountryCode: "GR", RegionID: "GR-M"})
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	require.NoError(t, h.applyRegion(r, testTenant(), s, nil, nil))
	assert.Equal(t, "GR-M", s.Fulfillment.RegionID)
	assert.Zero(t, asked)

	require.NoError(t, h.applyRegion(r, testTenant(), s, nil,
		[]SelectedFulfillmentOption{{OptionID: "acs:home_delivery"}}))
	assert.Empty(t, s.Fulfillment.RegionID)
	assert.Equal(t, "GR-14 (Αττική)", s.RegionChoices())
}

// A country the store delivers home to no region of is named at country.
func TestARefusedCountryIsNamedAtCountry(t *testing.T) {
	param, text := destinationRefusal(
		&checkout.Refusal{Code: django.CodeCountryNotServed})
	assert.Equal(t, "$.fulfillment_details.address.country", param)
	assert.Contains(t, text, "another country")
}

// A region the store refused at completion (its shipping zones no longer
// reach it) is named at state, not line_one.
func TestARefusedRegionIsNamedAtState(t *testing.T) {
	s := checkout.NewSession("public", testTenant().Domain, "acp",
		"29eb4495-e018-45e7-b59c-6646302bd4ef")
	applyFulfillment(s, &FulfillmentDetails{Address: &Address{
		LineOne: "Πανεπιστημίου 12", City: "Αθήνα",
		PostalCode: "10431", Country: "GR",
	}}, []SelectedFulfillmentOption{{OptionID: "acs:home_delivery"}})
	s.Buyer = checkout.Buyer{
		FirstName: "Μαρία", LastName: "Παπαδοπούλου",
		Email: "maria@example.test", Phone: "+306912345678",
	}
	s.DestinationRefused = &checkout.Refusal{
		Code: django.CodeRegionNotServed, Detail: "Not there."}
	s.Recompute()
	require.Equal(t, checkout.StatusIncomplete, s.Status)

	payload, err := Render(t.Context(), fixtureDjango(t),
		testTenant(), testLinks(testTenant()), s)
	require.NoError(t, err)
	var params []string
	for _, m := range payload.Messages {
		if m.Type == "error" {
			params = append(params, m.Code+" "+m.Param)
		}
	}
	assert.Equal(t,
		[]string{"invalid $.fulfillment_details.address.state"}, params)
}

package checkout

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

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// regionDjango serves the recorded regions for GR, which the store
// delivers to at home, one region for CY, which it does not, and none for
// any other country, as Django does.
func regionDjango(t *testing.T, status int) *django.Client {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join(
		"..", "..", "testdata", "fixtures", "django", "regions_gr.json"))
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/region/{country}/get_regions_by_country_alpha_2",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			served := r.URL.Query().Get("shippable") == "true"
			switch country := r.PathValue("country"); {
			case country == "GR":
				_, _ = w.Write(fixture)
			case country == "CY" && !served:
				_, _ = w.Write([]byte(`[{"alpha":"CY-01",` +
					`"translations":{"el":{"name":"Λευκωσία"}}}]`))
			default:
				_, _ = w.Write([]byte(`[]`))
			}
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "platform.test", "secret",
		time.Second, slog.New(slog.DiscardHandler), obs.NewMetrics())
}

func regionTenant() *tenant.Tenant {
	return &tenant.Tenant{
		DefaultLocale: "el",
		Domain:        "demostore.test",
	}
}

func TestResolveRegion(t *testing.T) {
	dj := regionDjango(t, http.StatusOK)
	for _, tc := range []struct {
		name, named, want string
	}{
		{"code", "GR-14", "GR-14"},
		{"code in another case", "gr-14", "GR-14"},
		{"name", "Αττική", "GR-14"},
		{"name in capitals, without its accent", "ΑΤΤΙΚΗ", "GR-14"},
		{"name in another language", "central macedonia", "GR-17"},
		{"surrounding space", "  Αττική ", "GR-14"},
		{"a region the country does not have", "Bavaria", ""},
		{"nothing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{Fulfillment: homeDelivery()}
			require.NoError(t, ResolveRegion(
				t.Context(), dj, regionTenant(), s, tc.named))
			assert.Equal(t, tc.want, s.Fulfillment.RegionID)
			assert.Len(t, s.RegionOptions, 2)
		})
	}
}

// A country written in any case is the store's upper-case code (Deliver),
// which its regions are found by, and the same address in another case is
// not another: a street line the store refused stays refused.
func TestACountryIsReadInAnyCase(t *testing.T) {
	f := homeDelivery()
	f.CountryCode = " gr "
	s := &Session{}
	s.Deliver(f)
	s.DestinationRefused = &Refusal{Code: django.CodeStreetNumberMissing}
	f.CountryCode = "Gr"
	s.Deliver(f)
	assert.NotNil(t, s.DestinationRefused, "the same address")
	require.NoError(t, ResolveRegion(
		t.Context(), regionDjango(t, http.StatusOK),
		regionTenant(), s, "Αττική"))
	assert.Equal(t, "GR", s.Fulfillment.CountryCode)
	assert.Equal(t, "GR-14", s.Fulfillment.RegionID)
}

// A region Django refused when the order was sent (the store stopped
// delivering there) is judged again against the regions the store serves
// now: the refusal goes, a served region is chosen, and one no longer
// served leaves the checkout asking for a region among the served ones.
func TestARefusedRegionIsJudgedAgain(t *testing.T) {
	dj := regionDjango(t, http.StatusOK)
	for _, tc := range []struct {
		name, named, want string
	}{
		{"another region the store serves", "GR-17", "GR-17"},
		{"a region it no longer serves", "GR-M", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{Fulfillment: homeDelivery()}
			s.DestinationRefused = &Refusal{Code: django.CodeRegionNotServed}
			require.NoError(t, ResolveRegion(
				t.Context(), dj, regionTenant(), s, tc.named))
			assert.Nil(t, s.DestinationRefused)
			assert.Equal(t, tc.want, s.Fulfillment.RegionID)
			assert.Equal(t, tc.want == "", s.regionMissing())
		})
	}

	// A street the store refused is not the region's to clear.
	s := &Session{Fulfillment: homeDelivery()}
	s.DestinationRefused = &Refusal{Code: django.CodeStreetNumberMissing}
	require.NoError(t, ResolveRegion(
		t.Context(), dj, regionTenant(), s, "GR-14"))
	assert.NotNil(t, s.DestinationRefused)
}

// A country the store delivers home to none of the regions of is refused,
// not read as one without regions: Django would price a checkout that
// names no region there and then refuse its order. The refusal holds
// through any other change of the address, and goes with the country or
// with a pickup.
func TestACountryWithNoServedRegionIsRefused(t *testing.T) {
	dj := regionDjango(t, http.StatusOK)
	f := homeDelivery()
	f.CountryCode = "CY"
	s := &Session{}
	resolve := func(named string) {
		t.Helper()
		s.Deliver(f)
		require.NoError(t, ResolveRegion(
			t.Context(), dj, regionTenant(), s, named))
	}

	resolve("CY-01")
	require.NotNil(t, s.DestinationRefused)
	assert.Equal(t, django.CodeCountryNotServed, s.DestinationRefused.Code)
	assert.Contains(t, s.Missing(), NeedDestination)
	assert.Empty(t, s.RegionOptions)

	f.AddressLine2 = "3ος όροφος"
	resolve("CY-01")
	assert.NotNil(t, s.DestinationRefused, "another line 2, same country")

	s.DestinationRefused = &Refusal{Code: django.CodeRegionNotServed}
	resolve("CY-01")
	assert.Equal(t, django.CodeCountryNotServed, s.DestinationRefused.Code)

	f.CountryCode = "GR"
	resolve("GR-14")
	assert.Nil(t, s.DestinationRefused, "another country")
	assert.Equal(t, "GR-14", s.Fulfillment.RegionID)

	f.CountryCode = "CY"
	resolve("CY-01")
	require.NotNil(t, s.DestinationRefused)
	f = Fulfillment{Kind: FulfillmentPickupPoint, ProviderCode: "boxnow",
		CountryCode: "CY", BoxnowLockerID: "25"}
	resolve("")
	assert.Nil(t, s.DestinationRefused, "a pickup")
}

// A country with no region at all, and one whose served regions come
// back, are not refused.
func TestACountryWithServedOrNoRegionsIsNotRefused(t *testing.T) {
	dj := regionDjango(t, http.StatusOK)
	for country, named := range map[string]string{"MT": "", "GR": "GR-14"} {
		f := homeDelivery()
		f.CountryCode = country
		s := &Session{Fulfillment: f}
		s.DestinationRefused = &Refusal{Code: django.CodeCountryNotServed}
		require.NoError(t, ResolveRegion(
			t.Context(), dj, regionTenant(), s, named))
		assert.Nil(t, s.DestinationRefused, country)
	}
}

// The refusal stays while the served regions cannot be read: nothing
// says the region is served again.
func TestARefusedRegionStaysWhileTheRegionsAreUnreadable(t *testing.T) {
	s := &Session{Fulfillment: homeDelivery()}
	s.DestinationRefused = &Refusal{Code: django.CodeRegionNotServed}
	require.Error(t, ResolveRegion(t.Context(),
		regionDjango(t, http.StatusInternalServerError), regionTenant(), s,
		"GR-14"))
	assert.NotNil(t, s.DestinationRefused)
}

// A pickup order is delivered to the point and takes no region: none
// is asked, and none chosen.
func TestAPickupDeliveryTakesNoRegion(t *testing.T) {
	s := &Session{Fulfillment: Fulfillment{
		Kind: FulfillmentPickupPoint, ProviderCode: "boxnow",
		CountryCode: "GR", BoxnowLockerID: "25",
	}}
	require.NoError(t, ResolveRegion(
		t.Context(), regionDjango(t, http.StatusInternalServerError),
		regionTenant(), s, "Αττική"))
	assert.Empty(t, s.Fulfillment.RegionID)
	assert.Empty(t, s.RegionOptions)
	assert.False(t, s.regionMissing())
}

func TestResolveRegionFirstMatchWins(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/region/GR/get_regions_by_country_alpha_2",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"alpha":"GR-14","translations":{"el":{"name":"Αττική"}}},
				{"alpha":"ATT","translations":{"el":{"name":"Αττική"}}}
			]`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dj := django.New(srv.URL+"/api/v1", "platform.test", "secret",
		time.Second, slog.New(slog.DiscardHandler), obs.NewMetrics())

	s := &Session{Fulfillment: homeDelivery()}
	require.NoError(t, ResolveRegion(
		t.Context(), dj, regionTenant(), s, "Αττική"))
	assert.Equal(t, "GR-14", s.Fulfillment.RegionID)
}

func TestResolveRegionCountryWithoutRegions(t *testing.T) {
	f := homeDelivery()
	f.CountryCode = "MT"
	s := &Session{Fulfillment: f, RegionOptions: []RegionOption{{Code: "GR-14"}}}
	require.NoError(t, ResolveRegion(
		t.Context(), regionDjango(t, http.StatusOK),
		regionTenant(), s, "anything"))

	assert.Empty(t, s.Fulfillment.RegionID)
	assert.Empty(t, s.RegionOptions, "a previous country's regions are gone")
	assert.False(t, s.regionMissing())
}

func TestResolveRegionNoCountryYet(t *testing.T) {
	s := &Session{}
	require.NoError(t, ResolveRegion(
		t.Context(), regionDjango(t, http.StatusBadGateway),
		regionTenant(), s, ""))
	assert.Empty(t, s.RegionOptions, "no country, no call")
}

// An unanswered lookup must fail the request: guessing that the country
// has no regions would report a session ready that Django then refuses.
func TestResolveRegionUpstreamFailure(t *testing.T) {
	s := &Session{Fulfillment: homeDelivery()}
	err := ResolveRegion(
		t.Context(), regionDjango(t, http.StatusBadGateway),
		regionTenant(), s, "GR-14")
	assert.ErrorIs(t, err, django.ErrUpstreamDown)
}

func TestRegionBlocksReadiness(t *testing.T) {
	regions := []RegionOption{
		{Code: "GR-14", Name: "Αττική"},
		{Code: "GR-17", Name: "Κεντρική Μακεδονία"},
	}
	s := &Session{
		Protocol: ProtocolUCP, Buyer: completeBuyer(),
		Fulfillment: homeDelivery(), PayWayID: 1, RegionOptions: regions,
	}

	s.Recompute()
	assert.Equal(t, StatusIncomplete, s.Status)
	assert.Equal(t, []Need{NeedRegion}, s.Missing())
	assert.Equal(t, "GR-14 (Αττική), GR-17 (Κεντρική Μακεδονία)",
		s.RegionChoices(), "the agent is told exactly what it can send")

	s.Fulfillment.RegionID = "GR-14"
	s.Recompute()
	assert.Equal(t, StatusReadyForComplete, s.Status)
	assert.Empty(t, s.Missing())
	assert.Empty(t, s.RegionChoices())
}

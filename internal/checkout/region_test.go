package checkout

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

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// regionDjango serves the recorded regions for GR and none for any other
// country, as Django does.
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
			if r.PathValue("country") == "GR" {
				_, _ = w.Write(fixture)
				return
			}
			_, _ = w.Write([]byte(`[]`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "platform.test", "secret",
		time.Second, slog.New(slog.DiscardHandler), obs.NewMetrics())
}

func regionTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{DefaultLocale: "el"},
		Domain:       "demostore.test",
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
				context.Background(), dj, regionTenant(), s, tc.named))
			assert.Equal(t, tc.want, s.Fulfillment.RegionID)
			assert.Len(t, s.RegionOptions, 2)
		})
	}
}

func TestResolveRegionReadsTheCountryInAnyCase(t *testing.T) {
	f := homeDelivery()
	f.CountryCode = "gr"
	s := &Session{Fulfillment: f}
	require.NoError(t, ResolveRegion(
		context.Background(), regionDjango(t, http.StatusOK),
		regionTenant(), s, "Αττική"))
	assert.Equal(t, "GR", s.Fulfillment.CountryCode)
	assert.Equal(t, "GR-14", s.Fulfillment.RegionID)
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
		context.Background(), dj, regionTenant(), s, "Αττική"))
	assert.Equal(t, "GR-14", s.Fulfillment.RegionID)
}

func TestResolveRegionCountryWithoutRegions(t *testing.T) {
	f := homeDelivery()
	f.CountryCode = "MT"
	s := &Session{Fulfillment: f, RegionOptions: []RegionOption{{Code: "GR-14"}}}
	require.NoError(t, ResolveRegion(
		context.Background(), regionDjango(t, http.StatusOK),
		regionTenant(), s, "anything"))

	assert.Empty(t, s.Fulfillment.RegionID)
	assert.Empty(t, s.RegionOptions, "a previous country's regions are gone")
	assert.False(t, s.regionMissing())
}

func TestResolveRegionNoCountryYet(t *testing.T) {
	s := &Session{}
	require.NoError(t, ResolveRegion(
		context.Background(), regionDjango(t, http.StatusBadGateway),
		regionTenant(), s, ""))
	assert.Empty(t, s.RegionOptions, "no country, no call")
}

// An unanswered lookup must fail the request: guessing that the country
// has no regions would report a session ready that Django then refuses.
func TestResolveRegionUpstreamFailure(t *testing.T) {
	s := &Session{Fulfillment: homeDelivery()}
	err := ResolveRegion(
		context.Background(), regionDjango(t, http.StatusBadGateway),
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
	assert.Equal(t,
		[]string{"fulfillment.regionId, one of: GR-14 (Αττική), " +
			"GR-17 (Κεντρική Μακεδονία)"},
		s.Missing(), "the agent is told exactly what it can send")

	s.Fulfillment.RegionID = "GR-14"
	s.Recompute()
	assert.Equal(t, StatusReadyForComplete, s.Status)
	assert.Empty(t, s.Missing())
	assert.Empty(t, s.RegionChoices())
}

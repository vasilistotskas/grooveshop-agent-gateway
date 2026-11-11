package mcpsrv

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

const (
	acsPath = "/api/v1/shipping/acs/stations/nearest"
)

// pickupDjango serves the store's shipping options (rows built with
// shippingRow) and the two pickup networks, and records each call's
// headers by path. A call carrying a cart token is answered with
// tokenStatus (401: Django refused the token).
func pickupDjango(
	t *testing.T, tokenStatus int, rows ...string,
) (*handlers, map[string][]http.Header) {
	t.Helper()
	fixture := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
			"fixtures", "django", name))
		require.NoError(t, err)
		return b
	}
	stations := fixture("acs_stations_nearest.json")
	locker := fixture("boxnow_nearest.json")
	seen := map[string][]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen[r.URL.Path] = append(seen[r.URL.Path], r.Header.Clone())
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("X-Cart-Token") != "" &&
				tokenStatus != http.StatusOK {
				w.WriteHeader(tokenStatus)
				_, _ = w.Write([]byte(`{"detail": "refused"}`))
				return
			}
			switch r.URL.Path {
			case optionsPath:
				_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
			case acsPath:
				_, _ = w.Write(stations)
			case nearestPath:
				_, _ = w.Write(locker)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.DiscardHandler)
	return &handlers{deps: Deps{Site: testSite, Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, nil)}}, seen
}

var (
	acsHome    = shippingRow("acs", "home_delivery", false, false)
	flatHome   = shippingRow("flat_rate", "home_delivery", false, false)
	acsLocker  = shippingRow("acs", "pickup_point", false, false)
	boxnowBox  = shippingRow("boxnow", "pickup_point", false, false)
	boxnowFull = shippingRow("boxnow", "pickup_point", false, true)
)

// athens is a full address: BOX NOW is searched with it.
var athens = PickupPointsIn{CountryCode: "GR", PostalCode: "10434",
	City: "Αθήνα", Street: "Σταδίου 1"}

func resultText(res *mcp.CallToolResult) string {
	return res.Content[0].(*mcp.TextContent).Text
}

// The demo store ships home delivery only, yet the tool searched both
// networks and the model offered BOX NOW lockers. A store without
// pickup delivery is told so, as an answer: no other postal code would
// find one.
func TestPickupPointsForAStoreWithoutPickupDelivery(t *testing.T) {
	h, seen := pickupDjango(t, http.StatusOK, flatHome)

	res, out, err := h.findPickupPoints(storeContext(), nil, athens)

	require.NoError(t, err)
	assert.Empty(t, out.Points)
	assert.Empty(t, out.Carriers)
	assert.Contains(t, resultText(res),
		"does not deliver to pickup points in GR")
	assert.Empty(t, seen[acsPath])
	assert.Empty(t, seen[nearestPath])
	require.Len(t, seen[optionsPath], 1)
}

// Only the carriers the store offers are searched.
func TestPickupPointsSearchOnlyTheStoresCarriers(t *testing.T) {
	h, seen := pickupDjango(t, http.StatusOK, acsHome, acsLocker)

	_, out, err := h.findPickupPoints(storeContext(), nil, athens)

	require.NoError(t, err)
	assert.Equal(t, []string{django.ShippingProviderACS}, out.Carriers)
	assert.NotEmpty(t, out.Points)
	assert.NotContains(t, providers(out), django.ShippingProviderBoxNow)
	assert.Len(t, seen[acsPath], 1)
	assert.Empty(t, seen[nearestPath], "the store has no BOX NOW")
}

// Django requires city, street and postal code for BOX NOW, so a lookup
// without the street was a certain 400: it is not made, and the model
// is told to ask the shopper.
func TestPickupPointsSkipBoxNowWithoutAStreet(t *testing.T) {
	for name, tc := range map[string]struct {
		rows       []string
		wantPoints bool
	}{
		"with ACS":     {[]string{acsLocker, boxnowBox}, true},
		"BOX NOW only": {[]string{boxnowBox}, false},
	} {
		t.Run(name, func(t *testing.T) {
			h, seen := pickupDjango(t, http.StatusOK, tc.rows...)

			res, out, err := h.findPickupPoints(storeContext(), nil,
				PickupPointsIn{CountryCode: "GR", PostalCode: "10434",
					City: "Αθήνα"})

			require.NoError(t, err)
			assert.Empty(t, seen[nearestPath])
			assert.NotContains(t, providers(out),
				django.ShippingProviderBoxNow)
			assert.Equal(t, tc.wantPoints, len(out.Points) > 0)
			assert.Contains(t, resultText(res), "city and street")
		})
	}
}

// A cart too large for every pickup carrier is told so, not searched.
func TestPickupPointsForACartNoPickupCarrierFits(t *testing.T) {
	h, seen := pickupDjango(t, http.StatusOK, acsHome, boxnowFull)
	ctx, _ := signedInWith(t)

	res, out, err := h.findPickupPoints(ctx, nil, athens)

	require.NoError(t, err)
	assert.Empty(t, out.Points)
	assert.Contains(t, resultText(res), "too large or too heavy")
	assert.Empty(t, seen[nearestPath])
}

// The nearest BOX NOW locker is looked up for a signed-in shopper's cart
// through their token; anywhere else it names no cart.
func TestPickupPointsNameTheSignedInShoppersCart(t *testing.T) {
	for name, tc := range map[string]struct {
		signedIn bool
	}{
		"signed in":     {true},
		"no cart token": {false},
	} {
		t.Run(name, func(t *testing.T) {
			h, seen := pickupDjango(t, http.StatusOK, acsLocker, boxnowBox)
			ctx := storeContext()
			if tc.signedIn {
				ctx, _ = signedInWith(t)
			}

			_, out, err := h.findPickupPoints(ctx, nil, athens)

			require.NoError(t, err)
			assert.Equal(t, []string{django.ShippingProviderACS,
				django.ShippingProviderBoxNow}, out.Carriers)
			assert.Contains(t, providers(out), django.ShippingProviderBoxNow)
			require.Len(t, seen[nearestPath], 1)
			hdr := seen[nearestPath][0]
			assert.Equal(t, tc.signedIn, hdr.Get("X-Cart-Token") != "")
			assert.Empty(t, hdr.Get("X-Cart-Id"))
		})
	}
}

// A refused token fails the lookup the way the cart tools report it,
// since the carriers are judged for the shopper's cart, and is marked on
// the token for the widget to mint a new one.
func TestPickupPointsReportARefusedCartToken(t *testing.T) {
	h, seen := pickupDjango(t, http.StatusUnauthorized, acsLocker, boxnowBox)
	ctx, tok := signedInWith(t)

	_, _, err := h.findPickupPoints(ctx, nil, athens)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cart could not be reached")
	assert.True(t, tok.Refused())
	assert.Empty(t, seen[nearestPath])
}

// A BOX NOW lookup that failed is an outage, not "no pickup points": with
// nothing else found the agent hears it is one. Points another network
// found still stand.
func TestPickupPointsReportABoxNowOutage(t *testing.T) {
	for name, tc := range map[string]struct {
		rows       []string
		acs        string
		wantPoints bool
	}{
		"BOX NOW only":        {[]string{boxnowBox}, "[]", false},
		"ACS found none":      {[]string{acsLocker, boxnowBox}, "[]", false},
		"ACS found its point": {[]string{acsLocker, boxnowBox}, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			stations, err := os.ReadFile(filepath.Join("..", "..",
				"testdata", "fixtures", "django", "acs_stations_nearest.json"))
			require.NoError(t, err)
			if tc.acs != "" {
				stations = []byte(tc.acs)
			}
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case optionsPath:
						_, _ = w.Write([]byte("[" +
							strings.Join(tc.rows, ",") + "]"))
					case acsPath:
						_, _ = w.Write(stations)
					default:
						w.WriteHeader(http.StatusBadGateway)
					}
				}))
			t.Cleanup(srv.Close)
			h := &handlers{deps: Deps{Site: testSite, Django: django.New(
				srv.URL+"/api/v1", "api.example.test", "secret",
				5*time.Second, slog.New(slog.DiscardHandler), nil)}}

			_, out, err := h.findPickupPoints(storeContext(), nil, athens)

			if tc.wantPoints {
				require.NoError(t, err)
				assert.NotEmpty(t, out.Points)
				return
			}
			var got *ToolError
			require.ErrorAs(t, err, &got)
			assert.Equal(t, "unavailable", got.Code)
		})
	}
}

func providers(out PickupPointsOut) []string {
	var p []string
	for _, pt := range out.Points {
		p = append(p, pt.Provider)
	}
	return p
}

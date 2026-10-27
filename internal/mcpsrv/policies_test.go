package mcpsrv

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

const (
	settingsPath   = "/api/v1/settings/public"
	returnPagePath = "/api/v1/content-page/return-policy"
)

// policiesDjango serves the store's public settings as settings (a JSON
// object) and its return-policy page with pageStatus (404: the store
// publishes none).
func policiesDjango(
	t *testing.T, settings string, pageStatus int,
) (*handlers, map[string][]http.Header) {
	t.Helper()
	seen := map[string][]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen[r.URL.Path] = append(seen[r.URL.Path], r.Header.Clone())
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case settingsPath:
				_, _ = w.Write([]byte(`{"settings": ` + settings + `}`))
			case returnPagePath:
				w.WriteHeader(pageStatus)
				if pageStatus == http.StatusOK {
					_, _ = w.Write([]byte(`{"id": 4, "slug": "return-policy"}`))
					return
				}
				_, _ = w.Write([]byte(`{"detail": "Not found."}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &handlers{deps: Deps{Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, nil)}}, seen
}

// The assistant answered "14 days" from its own knowledge. The terms
// come from the store: its settings and its published page.
func TestStorePoliciesStateTheStoresTerms(t *testing.T) {
	h, seen := policiesDjango(t,
		`{"RETURN_WINDOW_DAYS": "30", "RETURN_FEES": "customer",
		  "DISPATCH_CUTOFF": ""}`, http.StatusOK)

	res, out, err := h.getStorePolicies(storeContext(), nil,
		StorePoliciesIn{})

	require.NoError(t, err)
	assert.Equal(t, StorePoliciesOut{
		ReturnWindowDays: 30,
		ReturnFees:       django.ReturnFeesCustomer,
		ReturnPolicyURL:  "https://shop.example.test/return-policy",
	}, out)
	assert.Contains(t, resultText(res), "within 30 days")
	assert.Equal(t, "el", seen[settingsPath][0].Get("X-Language"))
}

// A term the store has not stated is absent, never a default, and an
// unpublished page is not linked.
func TestStorePoliciesLeaveOutWhatTheStoreHasNotStated(t *testing.T) {
	h, _ := policiesDjango(t,
		`{"RETURN_WINDOW_DAYS": "0", "RETURN_FEES": ""}`,
		http.StatusNotFound)

	res, out, err := h.getStorePolicies(storeContext(), nil,
		StorePoliciesIn{})

	require.NoError(t, err)
	assert.Equal(t, StorePoliciesOut{}, out)
	assert.Contains(t, resultText(res), "has not stated a return window")
	assert.Contains(t, resultText(res), "publishes no return policy page")
}

// A setting Django stops serving, or serves out of its declared shape,
// fails the call rather than read as "not stated".
func TestStorePoliciesRefuseUnreadableTerms(t *testing.T) {
	for name, settings := range map[string]string{
		"window missing": `{"RETURN_FEES": "free"}`,
		"window not days": `{"RETURN_WINDOW_DAYS": "two weeks",
			"RETURN_FEES": "free"}`,
		"fees unknown": `{"RETURN_WINDOW_DAYS": "14",
			"RETURN_FEES": "sometimes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := policiesDjango(t, settings, http.StatusOK)

			_, _, err := h.getStorePolicies(storeContext(), nil,
				StorePoliciesIn{})

			require.Error(t, err)
			assert.Contains(t, err.Error(), codeUnavailable)
		})
	}
}

// A page lookup that fails for any reason but "none published" is an
// outage, not an unpublished page.
func TestStorePoliciesReportAPageOutage(t *testing.T) {
	h, _ := policiesDjango(t,
		`{"RETURN_WINDOW_DAYS": "14", "RETURN_FEES": "free"}`,
		http.StatusBadGateway)

	_, _, err := h.getStorePolicies(storeContext(), nil, StorePoliciesIn{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), codeUnavailable)
}

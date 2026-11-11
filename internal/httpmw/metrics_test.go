package httpmw

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// net/http hands a route registered without a method any token as the
// method. The standard methods keep their own label; every other one,
// a lowercase spelling of a standard one included, shares "other", so
// the series of both instruments stay a fixed set.
func TestMetricsFoldsNonStandardMethods(t *testing.T) {
	m := obs.NewMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := Metrics(m)(mux)

	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace,
		"get", "BREW", "PROPFIND", "X-1",
	} {
		r := httptest.NewRequest(method, "/mcp", nil)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	want := map[string]float64{
		"GET": 1, "HEAD": 1, "POST": 1, "PUT": 1, "PATCH": 1, "DELETE": 1,
		"CONNECT": 1, "OPTIONS": 1, "TRACE": 1, "other": 4,
	}
	families, err := m.Registry.Gather()
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, f := range families {
		switch f.GetName() {
		case "gateway_http_requests_total",
			"gateway_http_request_duration_seconds":
		default:
			continue
		}
		seen[f.GetName()] = true
		got := map[string]float64{}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() != "method" {
					continue
				}
				if c := metric.GetCounter(); c != nil {
					got[l.GetValue()] += c.GetValue()
				} else {
					got[l.GetValue()] += float64(
						metric.GetHistogram().GetSampleCount())
				}
			}
		}
		assert.Equal(t, want, got, f.GetName())
	}
	assert.Len(t, seen, 2, "both instruments recorded the requests")
}

package acp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Request equivalence is the spec's table (§6.2), plus what "semantic
// JSON equality of the request body" and §6.7 (extension fields "MUST
// participate in body comparison") imply.
func TestFingerprintFollowsTheSpecsEquivalence(t *testing.T) {
	cases := []struct {
		name       string
		a, b       string
		equivalent bool
	}{
		{"different key ordering",
			`{"a":1,"b":{"c":2,"d":[3]}}`, `{"b":{"d":[3],"c":2},"a":1}`,
			true},
		{"null value vs absent key",
			`{"a":1,"b":null}`, `{"a":1}`, false},
		{"trailing zeros in numbers", `{"q":1.0}`, `{"q":1}`, true},
		{"a number's spelling", `{"q":10.50}`, `{"q":1.05e1}`, true},
		{"array element ordering", `{"a":[1,2]}`, `{"a":[2,1]}`, false},
		{"an extension field", `{"a":1}`, `{"a":1,"ext":{"x":true}}`,
			false},
		{"whitespace", "{\n  \"a\" : [ 1 , 2 ]\n}", `{"a":[1,2]}`, true},
		{"a string's escapes", `{"s":"é"}`, `{"s":"é"}`, true},
		{"another value", `{"a":1}`, `{"a":2}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := fingerprint([]byte(tc.a))
			require.NoError(t, err)
			b, err := fingerprint([]byte(tc.b))
			require.NoError(t, err)
			assert.Equal(t, tc.equivalent, a == b)
		})
	}

	// Cancel's body is optional: none at all is not an empty object.
	none, err := fingerprint(nil)
	require.NoError(t, err)
	empty, err := fingerprint([]byte(`{}`))
	require.NoError(t, err)
	assert.NotEqual(t, none, empty)
}

func TestAnOptionalBodyMayBeAbsentButNotMalformed(t *testing.T) {
	none, err := fingerprint(nil)
	require.NoError(t, err)
	for body, want := range map[string]int{
		"": http.StatusOK, "  \n": http.StatusOK,
		`{"intent_trace":{"reason_code":"other"}}`: http.StatusOK,
		"{": http.StatusBadRequest, "[]": http.StatusBadRequest,
	} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		rec := httptest.NewRecorder()
		hash, ok := decodeOptionalBody(rec, req, &struct{}{})
		if want == http.StatusBadRequest {
			assert.False(t, ok, "%q", body)
			specError(t, rec, http.StatusBadRequest, "invalid")
			continue
		}
		require.True(t, ok, "%q", body)
		if strings.TrimSpace(body) == "" {
			assert.Equal(t, none, hash, "%q", body)
		} else {
			assert.NotEqual(t, none, hash, "%q", body)
		}
	}
}

// A key keeps the answer of a request that did what it asked, unless it
// is a 5xx (§6.5), and the two 5xx answers of a completion whose order
// may exist while its stored session may not say so. A request that did
// nothing frees its key.
func TestWhichAnswersAKeyKeeps(t *testing.T) {
	cases := []struct {
		name                 string
		done, orderUnsettled bool
		status               int
		kept                 bool
	}{
		{"done", true, false, http.StatusOK, true},
		{"done, then not rendered", true, false,
			http.StatusServiceUnavailable, false},
		{"refused", false, false, http.StatusBadRequest, false},
		{"busy", false, false, http.StatusConflict, false},
		{"failed", false, false, http.StatusServiceUnavailable, false},
		{"answered by another request's session", false, false,
			http.StatusOK, false},
		{"an order placed", true, false, http.StatusOK, true},
		// The stored session says the order is placed: a retry run
		// afresh is answered with it.
		{"an order placed, not rendered", true, false,
			http.StatusServiceUnavailable, false},
		{"an order whose outcome is unknown", true, true,
			http.StatusBadGateway, true},
		{"an order whose session was not saved", true, true,
			http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.kept,
				keptAgainstKey(tc.done, tc.orderUnsettled, tc.status))
		})
	}
}

// What a held answer sends is what was written to it, headers included.
func TestAHeldResponseSendsWhatWasWritten(t *testing.T) {
	held := newHeldResponse()
	writeInFlight(held, idemRetryAfter, msgSessionBusy)
	rec := httptest.NewRecorder()
	rec.Header().Set("Idempotency-Key", "k")
	held.send(rec)
	specError(t, rec, http.StatusConflict, "idempotency_in_flight")
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
	assert.Equal(t, "k", rec.Header().Get("Idempotency-Key"))
	assert.Equal(t, held.body.String(), rec.Body.String())

	// Nothing written is net/http's empty 200.
	empty := httptest.NewRecorder()
	newHeldResponse().send(empty)
	assert.Equal(t, http.StatusOK, empty.Code)
}

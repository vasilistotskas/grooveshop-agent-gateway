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
		// The first spells é as a JSON escape: backslash, u, 00e9.
		{"a string's escapes", "{\"s\":\"\x5cu00e9\"}", `{"s":"é"}`, true},
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
		// null decodes as an object that names nothing, as it does for
		// every body; it is a body, so not the absent one's fingerprint.
		"null": http.StatusOK, "1": http.StatusBadRequest,
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

// A key keeps every answer that is final for its request, refusals
// included (§6.3). It frees a 5xx (§6.5) and the answers that ask for
// the same request again: 409 idempotency_in_flight and 429. It keeps
// the two 5xx answers of a completion whose order may exist while its
// stored session may not say so.
func TestWhichAnswersAKeyKeeps(t *testing.T) {
	cases := []struct {
		name           string
		orderUnsettled bool
		status         int
		kept           bool
	}{
		{"a 200", false, http.StatusOK, true},
		{"a 201", false, http.StatusCreated, true},
		{"a 400", false, http.StatusBadRequest, true},
		{"a 404", false, http.StatusNotFound, true},
		{"a 405", false, http.StatusMethodNotAllowed, true},
		{"a 409 in flight", false, http.StatusConflict, false},
		{"a 429", false, http.StatusTooManyRequests, false},
		{"a 500", false, http.StatusInternalServerError, false},
		{"a 502", false, http.StatusBadGateway, false},
		{"a 503", false, http.StatusServiceUnavailable, false},
		{"an order whose outcome is unknown", true,
			http.StatusBadGateway, true},
		{"an order whose session was not saved", true,
			http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.kept,
				keptAgainstKey(tc.orderUnsettled, tc.status))
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

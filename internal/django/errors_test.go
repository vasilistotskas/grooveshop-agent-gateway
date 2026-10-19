package django

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errorFrom sends one mutation to a Django answering status, headers and
// body, and returns the APIError it decoded.
func errorFrom(
	t *testing.T, status int, header http.Header, body string,
) *APIError {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range header {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)
	_, err := c.AddCartItem(context.Background(), "shop.example.test", "el",
		"", 1, 1)
	require.Error(t, err)
	apiErr, ok := errors.AsType[*APIError](err)
	require.True(t, ok, "got %T", err)
	return apiErr
}

// Django's error contract: `code` next to `detail`.
func TestAPIErrorReadsTheCodeAndDetail(t *testing.T) {
	e := errorFrom(t, http.StatusConflict, nil,
		`{"detail": "Μόνο 2 διαθέσιμα.", "code": "insufficient_stock"}`)

	assert.Equal(t, "insufficient_stock", e.Code)
	assert.Equal(t, "Μόνο 2 διαθέσιμα.", e.Detail)
	assert.ErrorIs(t, e, ErrConflict)
}

// A validation error has no detail: its fields' messages stand in,
// nested paths dotted, the non-field ones bare.
func TestAPIErrorReadsAValidationErrorsFields(t *testing.T) {
	e := errorFrom(t, http.StatusBadRequest, nil, `{
		"quantity": ["Ensure this value is less than or equal to 999."],
		"code": "max_value",
		"fields": {
			"quantity": [{"message": "Ensure this value is less than or equal to 999.", "code": "max_value"}],
			"nonFieldErrors": [{"message": "Cart is closed.", "code": "closed"}],
			"items": {"1": {"product": [{"message": "Invalid pk.", "code": "does_not_exist"}]}}
		}
	}`)

	assert.Equal(t, "max_value", e.Code)
	assert.Equal(t, "items.1.product: Invalid pk.; Cart is closed.; "+
		"quantity: Ensure this value is less than or equal to 999.", e.Detail)
}

// A request with a `code` field (a coupon) leaves the top-level `code`
// to that field's errors: the rest of the body still decodes.
func TestAPIErrorSurvivesACodeField(t *testing.T) {
	e := errorFrom(t, http.StatusBadRequest, nil, `{
		"code": ["This field may not be blank."],
		"fields": {"code": [{"message": "This field may not be blank.", "code": "blank"}]}
	}`)

	assert.Empty(t, e.Code)
	assert.Equal(t, "code: This field may not be blank.", e.Detail)
}

func TestAPIErrorReadsRetryAfter(t *testing.T) {
	e := errorFrom(t, http.StatusTooManyRequests,
		http.Header{"Retry-After": {"17"}},
		`{"detail": "Request was throttled.", "code": "throttled"}`)

	assert.Equal(t, 17*time.Second, e.RetryAfter)
	assert.ErrorIs(t, e, ErrThrottled)
}

// A body that is neither: no detail to show, but the log still quotes it.
func TestAPIErrorWithoutDetailQuotesTheBodyInItsMessageOnly(t *testing.T) {
	e := errorFrom(t, http.StatusBadRequest, nil, `["Not allowed."]`)

	assert.Empty(t, e.Detail)
	assert.Empty(t, e.Code)
	assert.Contains(t, e.Error(), `["Not allowed."]`)
}

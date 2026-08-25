package django

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyCouponReturnsUpdatedCart(t *testing.T) {
	var gotMethod, gotCartID, gotGateway string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/api/v1/cart/coupon", r.URL.Path)
			gotMethod = r.Method
			gotCartID = r.Header.Get("X-Cart-Id")
			gotGateway = r.Header.Get("X-Internal-Gateway")
			require.NoError(t,
				json.NewDecoder(r.Body).Decode(&gotBody))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "cart_with_coupon.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cart, err := c.ApplyCoupon(context.Background(),
		"shop.example.test", "el", "cart-uuid", "SAVE10")
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "cart-uuid", gotCartID)
	assert.Equal(t, "test-secret", gotGateway)
	assert.Equal(t, map[string]any{"code": "SAVE10"}, gotBody)

	assert.Equal(t, "92.94", cart.PromotionDiscount.String())
	assert.Equal(t, []string{"SAVE10"}, cart.AppliedCouponCodes)
	assert.False(t, cart.PromotionFreeShipping)
}

func TestApplyCouponRejectionSurfacesReason(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		reason string
		detail string
	}{
		{
			name:   "expired code",
			status: http.StatusBadRequest,
			body: `{"detail": "The promotion has ended",` +
				` "reason": "discount_code_expired"}`,
			reason: "discount_code_expired",
			detail: "The promotion has ended",
		},
		{
			name:   "unknown code",
			status: http.StatusBadRequest,
			body: `{"detail": "The code does not exist",` +
				` "reason": "discount_code_invalid"}`,
			reason: "discount_code_invalid",
			detail: "The code does not exist",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			_, err := c.ApplyCoupon(context.Background(),
				"shop.example.test", "el", "cart-uuid", "X")
			require.Error(t, err)

			var rejected *CouponRejectedError
			require.ErrorAs(t, err, &rejected)
			assert.Equal(t, tc.reason, rejected.Reason)
			assert.Equal(t, tc.detail, rejected.Detail)
			// Generic handlers still classify it as a validation error.
			assert.ErrorIs(t, err, ErrValidation)
		})
	}
}

func TestApplyCouponReasonlessBadRequestStaysAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code": ["This field is required."]}`))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ApplyCoupon(context.Background(),
		"shop.example.test", "el", "cart-uuid", "")
	require.Error(t, err)

	var rejected *CouponRejectedError
	assert.False(t, errors.As(err, &rejected),
		"a body without a reason must not map to a coupon rejection")
	assert.ErrorIs(t, err, ErrValidation)
}

func TestRemoveCouponReturnsUpdatedCart(t *testing.T) {
	var gotMethod, gotCartID string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/api/v1/cart/coupon", r.URL.Path)
			gotMethod = r.Method
			gotCartID = r.Header.Get("X-Cart-Id")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "cart_with_items.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cart, err := c.RemoveCoupon(context.Background(),
		"shop.example.test", "el", "cart-uuid")
	require.NoError(t, err)

	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "cart-uuid", gotCartID)
	assert.Empty(t, cart.AppliedCouponCodes)
	assert.Equal(t, "0.0", cart.PromotionDiscount.String())
}

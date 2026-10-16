package django

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureCartID is the cart every cart fixture serves.
const fixtureCartID = "29eb4495-e018-45e7-b59c-6646302bd4ef"

func TestApplyCouponReturnsUpdatedCart(t *testing.T) {
	var gotMethod, gotCartID, gotGateway string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/cart/coupon", r.URL.Path)
			gotMethod = r.Method
			gotCartID = r.Header.Get("X-Cart-Id")
			gotGateway = r.Header.Get("X-Internal-Gateway")
			assert.NoError(t,
				json.NewDecoder(r.Body).Decode(&gotBody))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "cart_with_coupon.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cart, err := c.ApplyCoupon(context.Background(),
		"shop.example.test", "el", fixtureCartID, "SAVE10")
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, fixtureCartID, gotCartID)
	assert.Equal(t, "test-secret", gotGateway)
	assert.Equal(t, map[string]any{"code": "SAVE10"}, gotBody)

	assert.Equal(t, "92.94", cart.PromotionDiscount.String())
	require.Len(t, cart.AppliedCoupons, 1)
	assert.Equal(t, "SAVE10", cart.AppliedCoupons[0].Code)
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
				"shop.example.test", "el", fixtureCartID, "X")
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
		"shop.example.test", "el", fixtureCartID, "")
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
			assert.Equal(t, "/api/v1/cart/coupon", r.URL.Path)
			gotMethod = r.Method
			gotCartID = r.Header.Get("X-Cart-Id")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "cart_with_items.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cart, err := c.RemoveCoupon(context.Background(),
		"shop.example.test", "el", fixtureCartID)
	require.NoError(t, err)

	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, fixtureCartID, gotCartID)
	assert.Empty(t, cart.AppliedCoupons)
	assert.Equal(t, "0.0", cart.PromotionDiscount.String())
}

// Django declares targetPrice a number: it must go on the wire as a JSON
// number, and a restock alert (no target) must not send the key at all.
func TestCreateProductAlertSendsTargetPriceAsNumber(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		target json.Number
		want   string
	}{
		{
			name: "price drop", kind: "price_drop", target: "19.90",
			want: `{"kind":"price_drop","product":7,` +
				`"email":"a@example.test","targetPrice":19.90}`,
		},
		{
			name: "restock", kind: "restock",
			want: `{"kind":"restock","product":7,` +
				`"email":"a@example.test"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "/api/v1/product/alert", r.URL.Path)
					var err error
					raw, err = io.ReadAll(r.Body)
					assert.NoError(t, err)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write(fixture(t, "product_alert_created.json"))
				}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			_, err := c.CreateProductAlert(context.Background(),
				"shop.example.test", "el", tc.kind, 7, "a@example.test",
				tc.target)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(raw))
			if tc.target != "" {
				assert.Contains(t, string(raw), `"targetPrice":19.90`,
					"a number, never a quoted string")
			}
		})
	}
}

// Django serves a new guest cart in place of one an anonymous caller
// cannot reach, and adds to that one: a call naming a cart fails when
// another cart answers it.
func TestCartCallsRefuseAnotherCartServedForTheNamedOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasPrefix(r.URL.Path, "/api/v1/cart/item") {
				_, _ = w.Write(fixture(t, "cart_item_created.json"))
				return
			}
			_, _ = w.Write(fixture(t, "cart_empty.json"))
		}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	ctx := context.Background()
	const host, other = "shop.example.test", "6f1c1f3e-2d6a-4b0f-9a3e-2f6d6c1b9a10"

	_, err := c.GetCart(ctx, host, "el", other)
	require.ErrorIs(t, err, ErrCartUnreachable)
	_, err = c.AddCartItem(ctx, host, "el", other, 1, 1)
	require.ErrorIs(t, err, ErrCartUnreachable)
	_, err = c.UpdateCartItem(ctx, host, "el", other, 410, 2)
	require.ErrorIs(t, err, ErrCartUnreachable)
	_, err = c.ApplyCoupon(ctx, host, "el", other, "SAVE10")
	require.ErrorIs(t, err, ErrCartUnreachable)
	_, err = c.RemoveCoupon(ctx, host, "el", other)
	require.ErrorIs(t, err, ErrCartUnreachable)

	cart, err := c.GetCart(ctx, host, "el", strings.ToUpper(fixtureCartID))
	require.NoError(t, err)
	assert.Equal(t, fixtureCartID, cart.UUID)
	_, err = c.GetCart(ctx, host, "el", "")
	require.NoError(t, err)
}

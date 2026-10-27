package mcpsrv

import (
	"context"
	"encoding/json"
	"io"
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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// couponDjango accepts SAVE10, refuses every other code the way the
// promotion engine does, and records what each call sent.
func couponDjango(t *testing.T, status int) (*handlers, *[]*http.Request) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata",
		"fixtures", "django", "cart_with_coupon.json"))
	require.NoError(t, err)
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			seen = append(seen, r)
			assert.Equal(t, "/api/v1/cart/coupon", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			switch {
			case status != http.StatusOK:
				w.WriteHeader(status)
			case r.Method == http.MethodPost && in.Code != "SAVE10":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"detail": "The promotion has ` +
					`ended", "reason": "discount_code_expired"}`))
			default:
				_, _ = w.Write(body)
			}
		}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &handlers{deps: Deps{Site: testSite, Django: django.New(srv.URL+"/api/v1",
		"api.example.test", "secret", 5*time.Second, log, nil)}}, &seen
}

// A code the shopper gave is applied to their cart, the new totals come
// back, and the turn records the change.
func TestApplyCouponReturnsTheNewTotals(t *testing.T) {
	h, seen := couponDjango(t, http.StatusOK)
	ctx, activity := listening(signedIn(t))

	_, out, err := h.applyCoupon(ctx, nil, ApplyCouponIn{Code: " SAVE10 "})

	require.NoError(t, err)
	assert.Equal(t, "92.94", out.PromotionDiscount)
	assert.Equal(t, []string{"SAVE10"}, out.AppliedCouponCodes)
	assert.True(t, activity.Mutated())
	require.Len(t, *seen, 1)
	assert.Equal(t, http.MethodPost, (*seen)[0].Method)
	assert.NotEmpty(t, (*seen)[0].Header.Get("X-Cart-Token"))
}

// The store's refusal reaches the model with its machine-readable
// reason and its words, and changes nothing.
func TestApplyCouponPassesTheRefusalThrough(t *testing.T) {
	h, _ := couponDjango(t, http.StatusOK)
	ctx, activity := listening(signedIn(t))

	_, _, err := h.applyCoupon(ctx, nil, ApplyCouponIn{Code: "OLD5"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discount_code_expired")
	assert.Contains(t, err.Error(), "The promotion has ended")
	assert.False(t, activity.Mutated())
}

func TestRemoveCouponReturnsTheNewTotals(t *testing.T) {
	h, seen := couponDjango(t, http.StatusOK)
	ctx, activity := listening(signedIn(t))

	_, out, err := h.removeCoupon(ctx, nil, RemoveCouponIn{})

	require.NoError(t, err)
	assert.Equal(t, fixtureCartID, out.CartID)
	assert.True(t, activity.Mutated())
	require.Len(t, *seen, 1)
	assert.Equal(t, http.MethodDelete, (*seen)[0].Method)
}

// A guest names the cart; a refused token is the unreachable cart.
func TestCouponToolsNeedACart(t *testing.T) {
	h, seen := couponDjango(t, http.StatusOK)
	guest := tenant.NewContext(context.Background(), cartTenant())

	_, _, err := h.applyCoupon(guest, nil, ApplyCouponIn{Code: "SAVE10"})
	assert.EqualError(t, err, "cartId and code are required")
	_, _, err = h.applyCoupon(signedIn(t), nil, ApplyCouponIn{Code: " "})
	assert.EqualError(t, err, "cartId and code are required")
	_, _, err = h.removeCoupon(guest, nil, RemoveCouponIn{})
	assert.EqualError(t, err, "cartId is required")
	assert.Empty(t, *seen)

	h, _ = couponDjango(t, http.StatusUnauthorized)
	ctx, activity := listening(signedIn(t))
	_, _, err = h.applyCoupon(ctx, nil, ApplyCouponIn{Code: "SAVE10"})
	assert.Contains(t, err.Error(), "could not be reached")
	_, _, err = h.removeCoupon(signedIn(t), nil, RemoveCouponIn{})
	assert.Contains(t, err.Error(), "could not be reached")
	assert.False(t, activity.Mutated())
}

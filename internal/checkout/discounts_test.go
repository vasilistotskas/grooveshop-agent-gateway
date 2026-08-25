package checkout

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// couponUpstream records coupon calls and answers them with the given
// status/body (200 answers serve a minimal cart).
type couponUpstream struct {
	mu      sync.Mutex
	applies []string // codes POSTed
	removes int      // DELETE calls

	applyStatus int
	applyBody   string
}

func (u *couponUpstream) client(t *testing.T) *django.Client {
	t.Helper()
	const cartJSON = `{"id":1,"uuid":"c-1","items":[],"totalPrice":0.0,` +
		`"totalDiscountValue":0.0,"promotionDiscount":0.0,` +
		`"promotionFreeShipping":false,"appliedCouponCodes":[],` +
		`"totalItems":0,"totalItemsUnique":0,"currency":"EUR"}`

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/cart/coupon",
		func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			u.mu.Lock()
			u.applies = append(u.applies, body.Code)
			u.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if u.applyStatus >= 400 {
				w.WriteHeader(u.applyStatus)
				_, _ = w.Write([]byte(u.applyBody))
				return
			}
			_, _ = w.Write([]byte(cartJSON))
		})
	mux.HandleFunc("DELETE /api/v1/cart/coupon",
		func(w http.ResponseWriter, _ *http.Request) {
			u.mu.Lock()
			u.removes++
			u.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(cartJSON))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics())
}

func TestApplyDiscountCodes(t *testing.T) {
	tn := pricingTenant()

	t.Run("first code applies, extras reject as combination", func(t *testing.T) {
		up := &couponUpstream{}
		dj := up.client(t)
		s := NewSession("public", tn.Domain, "acp", "c-1")

		err := ApplyDiscountCodes(context.Background(), dj, tn, s,
			[]string{"SAVE10", "EXTRA1", "EXTRA2"})
		require.NoError(t, err)

		assert.Equal(t, []string{"SAVE10"}, up.applies)
		assert.Zero(t, up.removes)
		assert.Equal(t, []string{"SAVE10", "EXTRA1", "EXTRA2"},
			s.DiscountCodes)
		require.Len(t, s.RejectedDiscounts, 2)
		for i, code := range []string{"EXTRA1", "EXTRA2"} {
			assert.Equal(t, code, s.RejectedDiscounts[i].Code)
			assert.Equal(t, DiscountCodeCombinationDisallowed,
				s.RejectedDiscounts[i].Reason)
		}
	})

	t.Run("empty list clears the coupon", func(t *testing.T) {
		up := &couponUpstream{}
		dj := up.client(t)
		s := NewSession("public", tn.Domain, "acp", "c-1")
		s.DiscountCodes = []string{"OLD"}
		s.RejectedDiscounts = []DiscountRejection{{Code: "stale"}}

		err := ApplyDiscountCodes(context.Background(), dj, tn, s, nil)
		require.NoError(t, err)

		assert.Empty(t, up.applies)
		assert.Equal(t, 1, up.removes)
		assert.Empty(t, s.DiscountCodes)
		assert.Empty(t, s.RejectedDiscounts)
	})

	t.Run("a refused code records the rejection, not an error",
		func(t *testing.T) {
			rejections := []struct {
				djangoReason string
				wantReason   string
			}{
				{"discount_code_invalid", "discount_code_invalid"},
				{"discount_code_expired", "discount_code_expired"},
				// Django-only reason folds onto the spec enum.
				{"discount_code_not_started", "discount_code_invalid"},
				{"discount_code_minimum_not_met",
					"discount_code_minimum_not_met"},
				{"discount_code_usage_limit_reached",
					"discount_code_usage_limit_reached"},
				{"discount_code_combination_disallowed",
					"discount_code_combination_disallowed"},
				{"discount_code_user_ineligible",
					"discount_code_user_ineligible"},
			}
			for _, tc := range rejections {
				t.Run(tc.djangoReason, func(t *testing.T) {
					up := &couponUpstream{
						applyStatus: http.StatusBadRequest,
						applyBody: `{"detail": "refused", "reason": "` +
							tc.djangoReason + `"}`,
					}
					dj := up.client(t)
					s := NewSession("public", tn.Domain, "acp", "c-1")

					err := ApplyDiscountCodes(context.Background(), dj, tn,
						s, []string{"NOPE"})
					require.NoError(t, err,
						"a rejection is session state, not a failure")

					require.Len(t, s.RejectedDiscounts, 1)
					rej := s.RejectedDiscounts[0]
					assert.Equal(t, "NOPE", rej.Code)
					assert.Equal(t, tc.wantReason, rej.Reason)
					assert.Equal(t, "refused", rej.Message)
					// Replace semantics: any lingering coupon is cleared.
					assert.Equal(t, 1, up.removes)
				})
			}
		})

	t.Run("rejected first code keeps extras' rejections too",
		func(t *testing.T) {
			up := &couponUpstream{
				applyStatus: http.StatusBadRequest,
				applyBody: `{"detail": "gone",` +
					` "reason": "discount_code_expired"}`,
			}
			dj := up.client(t)
			s := NewSession("public", tn.Domain, "acp", "c-1")

			err := ApplyDiscountCodes(context.Background(), dj, tn, s,
				[]string{"DEAD", "EXTRA"})
			require.NoError(t, err)
			require.Len(t, s.RejectedDiscounts, 2)
			assert.Equal(t, "DEAD", s.RejectedDiscounts[0].Code)
			assert.Equal(t, "discount_code_expired",
				s.RejectedDiscounts[0].Reason)
			assert.Equal(t, "EXTRA", s.RejectedDiscounts[1].Code)
		})

	t.Run("upstream failure surfaces as an error", func(t *testing.T) {
		up := &couponUpstream{
			applyStatus: http.StatusInternalServerError,
			applyBody:   `{"detail": "boom"}`,
		}
		dj := up.client(t)
		s := NewSession("public", tn.Domain, "acp", "c-1")

		err := ApplyDiscountCodes(context.Background(), dj, tn, s,
			[]string{"SAVE10"})
		require.Error(t, err)
		assert.ErrorIs(t, err, django.ErrUpstreamDown)
		assert.Empty(t, s.RejectedDiscounts)
	})
}

func TestMapDiscountReason(t *testing.T) {
	assert.Equal(t, "discount_code_expired",
		mapDiscountReason("discount_code_expired"))
	assert.Equal(t, DiscountCodeInvalid,
		mapDiscountReason("discount_code_not_started"))
	assert.Equal(t, DiscountCodeInvalid,
		mapDiscountReason("something_new_from_upstream"))
}

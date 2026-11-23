package checkout

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

func TestAddDiscounts(t *testing.T) {
	t.Run("each offer keeps its own amount", func(t *testing.T) {
		p := &Pricing{}
		require.NoError(t, p.addDiscounts(&django.Quote{
			AppliedPromotions: []django.QuotePromotion{
				{PromotionID: 3, Name: "2+1", Amount: json.Number("10.79")},
				{
					PromotionID: 5, Name: "-5 €", Code: "SAVE5",
					Amount: json.Number("5.00"),
				},
			},
			AppliedCoupons: []django.AppliedCoupon{{Code: "SAVE5"}},
		}))

		assert.Equal(t, []PricedDiscount{
			{PromotionID: 3, Name: "2+1", Amount: 1079},
			{PromotionID: 5, Name: "-5 €", Code: "SAVE5", Amount: 500},
		}, p.Discounts)
		assert.Empty(t, p.RefusedCoupons)
	})

	t.Run("a free-shipping coupon applies at 0", func(t *testing.T) {
		p := &Pricing{}
		require.NoError(t, p.addDiscounts(&django.Quote{
			AppliedCoupons: []django.AppliedCoupon{{Code: "SHIPFREE"}},
		}))

		assert.Equal(t, []PricedDiscount{{Name: "SHIPFREE", Code: "SHIPFREE"}},
			p.Discounts)
	})

	t.Run("the coupon the quote blocks is a rejection in the spec "+
		"vocabulary, whatever its verdict says of it", func(t *testing.T) {
		p := &Pricing{}
		require.NoError(t, p.addDiscounts(&django.Quote{
			AppliedPromotions: []django.QuotePromotion{
				{PromotionID: 3, Name: "2+1", Amount: json.Number("10.79")},
			},
			BlockingCoupon: &django.QuoteBlockingCoupon{
				Code: "FIRST10", Reason: "discount_code_not_applicable",
			},
			AppliedCoupons: []django.AppliedCoupon{{Code: "FIRST10"}},
		}))

		assert.Equal(t, []PricedDiscount{
			{PromotionID: 3, Name: "2+1", Amount: 1079},
		}, p.Discounts)
		assert.Equal(t, []DiscountRejection{
			{Code: "FIRST10", Reason: DiscountCodeInvalid},
		}, p.RefusedCoupons)
		require.NotNil(t, p.CouponRefused,
			"the order is refused for it: the checkout is not ready")
		assert.Equal(t, p.CouponRefused, p.Refused())
	})

	t.Run("a coupon the quote refuses without blocking the order is a "+
		"rejection too", func(t *testing.T) {
		p := &Pricing{}
		require.NoError(t, p.addDiscounts(&django.Quote{
			AppliedCoupons: []django.AppliedCoupon{{
				Code: "WEAK", Rejection: DiscountCodeCombinationDisallowed,
			}},
		}))

		assert.Empty(t, p.Discounts)
		assert.Equal(t, []DiscountRejection{
			{Code: "WEAK", Reason: DiscountCodeCombinationDisallowed},
		}, p.RefusedCoupons)
	})
}

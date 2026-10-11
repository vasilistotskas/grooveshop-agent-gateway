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
		require.NoError(t, p.addDiscounts(&django.Cart{
			AppliedPromotions: []django.AppliedPromotion{
				{PromotionID: 3, Name: "2+1", Amount: json.Number("10.79")},
				{
					PromotionID: 5, Name: "-5 €", Code: "SAVE5",
					Amount: json.Number("5.00"),
				},
			},
			AppliedCoupons: []django.AppliedCoupon{
				{Code: "SAVE5", Amount: json.Number("5.00")},
			},
		}))

		assert.Equal(t, []PricedDiscount{
			{PromotionID: 3, Name: "2+1", Amount: 1079},
			{PromotionID: 5, Name: "-5 €", Code: "SAVE5", Amount: 500},
		}, p.Discounts)
		assert.Empty(t, p.RefusedCoupons)
	})

	t.Run("a free-shipping coupon applies at 0", func(t *testing.T) {
		p := &Pricing{}
		require.NoError(t, p.addDiscounts(&django.Cart{
			AppliedCoupons: []django.AppliedCoupon{{
				Code: "SHIPFREE", Amount: json.Number("0"),
				FreeShipping: true,
			}},
		}))

		assert.Equal(t, []PricedDiscount{{Name: "SHIPFREE", Code: "SHIPFREE"}},
			p.Discounts)
	})

	t.Run("a refused coupon is a rejection in the spec vocabulary",
		func(t *testing.T) {
			p := &Pricing{}
			require.NoError(t, p.addDiscounts(&django.Cart{
				AppliedPromotions: []django.AppliedPromotion{
					{PromotionID: 3, Name: "2+1", Amount: json.Number("10.79")},
				},
				AppliedCoupons: []django.AppliedCoupon{
					{
						Code: "GAN20", Amount: json.Number("0"),
						Rejection: "discount_code_not_applicable",
					},
					{
						Code: "WEAK", Amount: json.Number("0"),
						Rejection: DiscountCodeCombinationDisallowed,
					},
				},
			}))

			assert.Equal(t, []PricedDiscount{
				{PromotionID: 3, Name: "2+1", Amount: 1079},
			}, p.Discounts)
			assert.Equal(t, []DiscountRejection{
				{Code: "GAN20", Reason: DiscountCodeInvalid},
				{Code: "WEAK", Reason: DiscountCodeCombinationDisallowed},
			}, p.RefusedCoupons)
		})
}

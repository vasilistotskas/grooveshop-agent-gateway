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
			AppliedPromotions: []django.AppliedPromotion{
				{PromotionID: 3, Name: "2+1", Amount: json.Number("10.79")},
				{
					PromotionID: 5, Name: "-5 €", Code: "SAVE5",
					Amount: json.Number("5.00"),
				},
			},
		}, &django.Cart{
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
		require.NoError(t, p.addDiscounts(&django.Quote{}, &django.Cart{
			AppliedCoupons: []django.AppliedCoupon{{
				Code: "SHIPFREE", Amount: json.Number("0"),
				FreeShipping: true,
			}},
		}))

		assert.Equal(t, []PricedDiscount{{Name: "SHIPFREE", Code: "SHIPFREE"}},
			p.Discounts)
	})

	t.Run("the coupon the quote blocks is a rejection in the spec "+
		"vocabulary, whatever the cart says of it", func(t *testing.T) {
		p := &Pricing{}
		require.NoError(t, p.addDiscounts(&django.Quote{
			AppliedPromotions: []django.AppliedPromotion{
				{PromotionID: 3, Name: "2+1", Amount: json.Number("10.79")},
			},
			BlockingCoupon: &django.QuoteBlockingCoupon{
				Code: "FIRST10", Reason: "discount_code_not_applicable",
			},
		}, &django.Cart{
			AppliedCoupons: []django.AppliedCoupon{
				{Code: "FIRST10", Amount: json.Number("0"), FreeShipping: true},
			},
		}))

		assert.Equal(t, []PricedDiscount{
			{PromotionID: 3, Name: "2+1", Amount: 1079},
		}, p.Discounts)
		assert.Equal(t, []DiscountRejection{
			{Code: "FIRST10", Reason: DiscountCodeInvalid},
		}, p.RefusedCoupons)
	})
}

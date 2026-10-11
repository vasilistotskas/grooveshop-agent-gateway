package acp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
)

func compileDiscounts(t *testing.T) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "schemas", "acp",
		"2026-04-17", "schema.discount.json")
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	raw, err := jsonschema.UnmarshalJSON(f)
	require.NoError(t, err)

	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("acp-discount.json", raw))
	schema, err := c.Compile("acp-discount.json#/$defs/discounts_response")
	require.NoError(t, err)
	return schema
}

func TestRenderDiscounts(t *testing.T) {
	schema := compileDiscounts(t)

	// A staging cart read "SAVE5 −39,98 €" on the storefront when SAVE5
	// was worth 5,00 € and automatic offers the rest; the ACP block made
	// the same claim by crediting the whole total to the first code.
	t.Run("each applied discount carries its own amount", func(t *testing.T) {
		s := &checkout.Session{DiscountCodes: []string{"SAVE5"}}
		out := &Session{}
		renderDiscounts(s, &checkout.Pricing{
			DiscountTotal: 1579,
			Discounts: []checkout.PricedDiscount{
				{PromotionID: 3, Name: "2+1", Amount: 1079},
				{PromotionID: 5, Name: "-5 €", Code: "SAVE5", Amount: 500},
			},
		}, out)

		require.NoError(t, schema.Validate(roundTrip(t, out.Discounts)))
		assert.Equal(t, []AppliedDiscount{
			{
				ID: "promotion_3", Coupon: Coupon{ID: "promotion_3", Name: "2+1"},
				Amount: 1079, Automatic: true,
			},
			{
				ID: "SAVE5", Code: "SAVE5",
				Coupon: Coupon{ID: "SAVE5", Name: "-5 €"}, Amount: 500,
			},
		}, out.Discounts.Applied)
		assert.Empty(t, out.Discounts.Rejected)
	})

	t.Run("an attached code the store refuses is rejected, not applied",
		func(t *testing.T) {
			s := &checkout.Session{}
			out := &Session{}
			renderDiscounts(s, &checkout.Pricing{
				DiscountTotal: 1079,
				Discounts: []checkout.PricedDiscount{
					{PromotionID: 3, Name: "2+1", Amount: 1079},
				},
				RefusedCoupons: []checkout.DiscountRejection{
					{Code: "GAN20", Reason: checkout.DiscountCodeInvalid},
				},
			}, out)

			require.NoError(t, schema.Validate(roundTrip(t, out.Discounts)))
			require.Len(t, out.Discounts.Applied, 1)
			assert.True(t, out.Discounts.Applied[0].Automatic)
			assert.Equal(t, []RejectedDiscount{
				{Code: "GAN20", Reason: checkout.DiscountCodeInvalid},
			}, out.Discounts.Rejected)
			require.Len(t, out.Messages, 1)
			assert.Equal(t, checkout.DiscountCodeInvalid, out.Messages[0].Code)
		})

	t.Run("a code refused at submission is reported once", func(t *testing.T) {
		s := &checkout.Session{
			DiscountCodes: []string{"GAN20"},
			RejectedDiscounts: []checkout.DiscountRejection{{
				Code: "GAN20", Reason: checkout.DiscountCodeInvalid,
				Message: "No item in the cart qualifies for this promotion",
			}},
		}
		out := &Session{}
		renderDiscounts(s, &checkout.Pricing{
			RefusedCoupons: []checkout.DiscountRejection{
				{Code: "GAN20", Reason: checkout.DiscountCodeInvalid},
			},
		}, out)

		require.NoError(t, schema.Validate(roundTrip(t, out.Discounts)))
		require.Len(t, out.Discounts.Rejected, 1)
		assert.Equal(t, "No item in the cart qualifies for this promotion",
			out.Discounts.Rejected[0].Message)
	})
}

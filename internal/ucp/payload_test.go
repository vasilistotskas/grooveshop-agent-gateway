package ucp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
)

const fixtureCart = "29eb4495-e018-45e7-b59c-6646302bd4ef"

func testBuilder(t *testing.T, cartFixture ...string) *Builder {
	t.Helper()
	return NewBuilder(fixtureDjango(t, cartFixture...),
		"https://{assets_host}/img/{path}.webp", "assets.platform.test",
		"production", storefront.NewSite("el"))
}

// built renders s, checks the payload against checkout.json composed with
// the discount extension, and returns it as a platform decodes it.
func built(
	t *testing.T, b *Builder, s *checkout.Session,
	caps map[string][]CapabilityRef,
) map[string]any {
	t.Helper()
	payload, err := b.BuildCheckout(context.Background(), testTenant(), s, caps)
	require.NoError(t, err)
	require.NoError(t, checkoutSchema(t).Validate(roundTrip(t, payload)))
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// With the extension negotiated, the checkout carries `discounts`: the
// codes it holds, echoed so that a platform sending the checkout back
// keeps them, and what the cart applies, each with its amount. A refused
// code is a warning at its place in codes, with the rejection's code.
func TestCheckoutRendersTheDiscountExtension(t *testing.T) {
	b := testBuilder(t, "cart_with_coupon.json")
	s := checkout.NewSession("public", "shop.example.test", "ucp", fixtureCart)
	s.DiscountCodes = []string{"SAVE10", "EXTRA5"}
	s.RejectedDiscounts = []checkout.DiscountRejection{{
		Code:    "EXTRA5",
		Reason:  checkout.DiscountCodeCombinationDisallowed,
		Message: "Only one discount code can be applied per order.",
	}}

	out := built(t, b, s, checkoutCaps)
	assert.Equal(t, map[string]any{
		"codes": []any{"SAVE10", "EXTRA5"},
		"applied": []any{map[string]any{
			"code": "SAVE10", "title": "Save 10%", "amount": float64(9294),
		}},
	}, out["discounts"])
	assert.Contains(t, out["messages"], map[string]any{
		"type": "warning", "code": "discount_code_combination_disallowed",
		"path":    "$.discounts.codes[1]",
		"content": "Only one discount code can be applied per order.",
	})

	// An automatic promotion has no code and says so.
	s.DiscountCodes, s.RejectedDiscounts = nil, nil
	pricing := &checkout.Pricing{Discounts: []checkout.PricedDiscount{
		{PromotionID: 3, Name: "Summer", Amount: 500},
	}}
	auto := &Checkout{}
	renderDiscounts(s, pricing, auto)
	assert.Equal(t, &Discounts{Applied: []AppliedDiscount{
		{Title: "Summer", Amount: 500, Automatic: true},
	}}, auto.Discounts)
}

// Without the extension negotiated the checkout has no `discounts` and
// no warnings about codes: its member is not part of this response.
func TestCheckoutWithoutTheExtensionRendersNoDiscounts(t *testing.T) {
	b := testBuilder(t, "cart_with_coupon.json")
	s := checkout.NewSession("public", "shop.example.test", "ucp", fixtureCart)
	s.DiscountCodes = []string{"SAVE10", "EXTRA5"}
	s.RejectedDiscounts = []checkout.DiscountRejection{{
		Code: "EXTRA5", Reason: checkout.DiscountCodeCombinationDisallowed,
	}}

	out := built(t, b, s, plainCaps)
	assert.NotContains(t, out, "discounts")
	for _, m := range out["messages"].([]any) {
		assert.NotEqual(t, "warning", m.(map[string]any)["type"])
	}
	// The cart's discount still shapes the totals: they are core.
	assert.Contains(t, out["totals"], map[string]any{
		"type": "discount", "display_text": "Discount",
		"amount": float64(-9294),
	})
}

// continue_url is in every non-terminal status the spec asks it for and
// in no terminal one. An open checkout's is the claim page of its cart.
// One being placed gets none: its order may exist, and the cart could be
// ordered again.
func TestContinueURLFollowsTheStatus(t *testing.T) {
	b := testBuilder(t)
	claim := "https://shop.example.test/cart/claim?uuid=" + fixtureCart
	order := "https://shop.example.test/checkout/success/" +
		"b9be45e5-6062-4976-ae7b-2c31eb2ad689"
	for _, tc := range []struct {
		status checkout.Status
		placed bool
		want   string
	}{
		{checkout.StatusIncomplete, false, claim},
		{checkout.StatusReadyForComplete, false, claim},
		{checkout.StatusCompleteInProgress, false, ""},
		{checkout.StatusRequiresEscalation, true, order},
		{checkout.StatusCompleted, true, ""},
		{checkout.StatusCanceled, false, ""},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			s := checkout.NewSession("public", "shop.example.test", "ucp",
				fixtureCart)
			s.Status = tc.status
			if tc.placed {
				s.OrderID = 684
				s.OrderUUID = "b9be45e5-6062-4976-ae7b-2c31eb2ad689"
			}
			out := built(t, b, s, checkoutCaps)
			if tc.want == "" {
				assert.NotContains(t, out, "continue_url")
				return
			}
			assert.Equal(t, tc.want, out["continue_url"])
		})
	}
}

// The discount extension is advertised by a store that takes codes, and
// by no other: a store with promotions off would refuse every code.
func TestProfileAdvertisesTheDiscountExtensionWithPromotions(t *testing.T) {
	schema := compileUCP(t, "profile.json#/$defs/business_schema")
	tn := testTenant()
	assert.NotContains(t, BusinessCapabilities(tn), CapabilityDiscount)

	tn.Features.Promotions = true
	profile := BuildProfile(tn, testKey(t), "production")
	require.NoError(t, schema.Validate(roundTrip(t, profile)))
	assert.Equal(t, []Capability{{
		Version: Version,
		Spec:    "https://ucp.dev/" + Version + "/specification/shopping/extensions/discount/",
		Schema:  "https://ucp.dev/" + Version + "/schemas/shopping/discount.json",
		Extends: CapabilityCheckout,
	}}, profile.UCP.Capabilities[CapabilityDiscount])
}

package mcpsrv

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func cartTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName:      "public",
			DefaultLocale:   "el",
			DefaultCurrency: "EUR",
		},
		Domain: "shop.example.test",
	}
}

func TestCartOutDiscountFields(t *testing.T) {
	h := &handlers{}
	tn := cartTenant()

	t.Run("zero-discount cart omits the discount fields", func(t *testing.T) {
		out := h.cartOut(tn, &django.Cart{
			UUID:               "c-1",
			TotalPrice:         json.Number("929.36"),
			PromotionDiscount:  json.Number("0.0"),
			TotalDiscountValue: json.Number("0.0"),
		})
		assert.Empty(t, out.PromotionDiscount)
		assert.Empty(t, out.TotalDiscountValue)
		assert.Empty(t, out.AppliedCouponCodes)
		assert.False(t, out.FreeShipping)
	})

	t.Run("coupon cart surfaces discount state", func(t *testing.T) {
		out := h.cartOut(tn, &django.Cart{
			UUID:                  "c-1",
			TotalPrice:            json.Number("929.36"),
			PromotionDiscount:     json.Number("92.94"),
			TotalDiscountValue:    json.Number("10.00"),
			PromotionFreeShipping: true,
			AppliedCouponCodes:    []string{"SAVE10"},
		})
		assert.Equal(t, "92.94", out.PromotionDiscount)
		assert.Equal(t, "10.00", out.TotalDiscountValue)
		assert.Equal(t, []string{"SAVE10"}, out.AppliedCouponCodes)
		assert.True(t, out.FreeShipping)

		summary := h.cartSummary(out)
		tc, ok := summary.Content[0].(*mcp.TextContent)
		require.True(t, ok)
		assert.Contains(t, tc.Text, "92.94")
		assert.Contains(t, tc.Text, "SAVE10")
		assert.Contains(t, tc.Text, "Shipping is free")
	})
}

func TestPosNum(t *testing.T) {
	cases := []struct {
		in   json.Number
		want string
	}{
		{"", ""},
		{"0", ""},
		{"0.0", ""},
		{"0.00", ""},
		{"-5.00", ""},
		{"92.94", "92.94"},
		{"5", "5"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, posNum(tc.in), "posNum(%q)", tc.in)
	}
}

package django

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests decode fixtures recorded from the real dev API. They are the
// drift guard for the hand-written DTOs: refresh the fixtures whenever
// schema.yml changes an endpoint the gateway consumes, and these tests fail
// loudly if a used field moves or changes type.

func decodeFixture[T any](t *testing.T, name string) T {
	t.Helper()
	var out T
	dec := json.NewDecoder(bytes.NewReader(fixture(t, name)))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&out))
	return out
}

func TestDecodeProductDetail(t *testing.T) {
	p := decodeFixture[Product](t, "product_detail.json")

	assert.Equal(t, int64(1), p.ID)
	assert.Equal(t, "including-hope", p.Slug)
	assert.Equal(t, int64(262), p.Category)
	assert.Equal(t, "464.68", p.FinalPrice.String())
	assert.Equal(t, "387.23", p.Price.String())
	assert.Equal(t, "20.0", p.VatPercent.String())
	assert.Equal(t, 186, p.Stock)
	assert.True(t, p.Active)
	require.NotNil(t, p.Weight)
	assert.Equal(t, "g", p.Weight.Unit)
	assert.Equal(t, "Portable Phone Charger", p.Translations["el"].Name)
	assert.False(t, p.PriceDropAlertsEnabled)
}

func TestDecodeProductVariants(t *testing.T) {
	v := decodeFixture[ProductVariants](t, "product_variants.json")
	require.Len(t, v.Variants, 1)
	assert.Equal(t, int64(1), v.Variants[0].ID)
}

func TestDecodeProductReviewsPage(t *testing.T) {
	page := decodeFixture[Page[Review]](t, "product_reviews.json")

	assert.Equal(t, int64(1), page.Count)
	require.Len(t, page.Results, 1)
	r := page.Results[0]
	assert.Equal(t, 5, r.Rate)
	assert.Equal(t, "Willie", r.User.FirstName)
	assert.NotEmpty(t, r.Translations["el"].Comment)
}

func TestDecodeSearchProduct(t *testing.T) {
	res := decodeFixture[SearchProductResponse](t, "search_product.json")

	assert.Equal(t, int64(123270), res.EstimatedTotalHits)
	require.Len(t, res.Results, 2)
	hit := res.Results[0]
	assert.Equal(t, int64(1528), hit.ID)
	assert.Equal(t, int64(510), hit.Master)
	assert.Equal(t, "934.61", hit.FinalPrice.String())
	assert.Equal(t, "el", hit.LanguageCode)
	assert.Equal(t, "product", hit.ContentType)
	assert.Equal(t, 250, hit.Stock)
}

func TestDecodeTrending(t *testing.T) {
	res := decodeFixture[TrendingResponse](t, "search_trending.json")
	assert.Equal(t, 24, res.WindowHours)
	require.NotEmpty(t, res.Results)
	assert.Equal(t, "phone", res.Results[0].Query)
}

func TestDecodeCategories(t *testing.T) {
	cats := decodeFixture[[]Category](t, "categories_all.json")
	require.NotEmpty(t, cats)
	assert.Equal(t, int64(1), cats[0].ID)
	assert.Equal(t, "Bags & Luggage", cats[0].Translations["el"].Name)
	assert.Equal(t, 0, cats[0].Level)
}

func TestDecodeCartWithItems(t *testing.T) {
	c := decodeFixture[Cart](t, "cart_with_items.json")

	assert.Equal(t, "29eb4495-e018-45e7-b59c-6646302bd4ef", c.UUID)
	require.Len(t, c.Items, 1)
	assert.Equal(t, "929.36", c.TotalPrice.String())
	assert.Equal(t, "0.0", c.PromotionDiscount.String())
	assert.False(t, c.PromotionFreeShipping)
	assert.Empty(t, c.AppliedCouponCodes)
	assert.Equal(t, "0.0", c.TotalDiscountValue.String())
}

func TestDecodeCartWithCoupon(t *testing.T) {
	c := decodeFixture[Cart](t, "cart_with_coupon.json")

	assert.Equal(t, "92.94", c.PromotionDiscount.String())
	assert.Equal(t, []string{"SAVE10"}, c.AppliedCouponCodes)
	assert.False(t, c.PromotionFreeShipping)
	assert.Equal(t, "929.36", c.TotalPrice.String())
}

func TestDecodeOrderPricingBreakdown(t *testing.T) {
	o := decodeFixture[Order](t, "order_by_uuid.json")

	pb := o.PricingBreakdown
	assert.Equal(t, "64.67", pb.ItemsSubtotal.String())
	assert.Equal(t, "5.0", pb.Discount.String())
	assert.Equal(t, "0", pb.LoyaltyDiscount.String())
	assert.Equal(t, "0", pb.GiftCardAmount.String())
	assert.Equal(t, "64.17", pb.GrandTotal.String())
	assert.Equal(t, "EUR", pb.Currency)
}

func TestDecodePayWays(t *testing.T) {
	page := decodeFixture[Page[PayWay]](t, "pay_way.json")

	require.Len(t, page.Results, 2)
	viva := page.Results[1]
	assert.Equal(t, "viva_wallet", viva.ProviderCode)
	assert.True(t, viva.IsOnlinePayment)
	assert.Equal(t, "1.0", viva.Cost.String())
	cod := page.Results[0]
	assert.Equal(t, "cash_on_delivery", cod.ProviderCode)
	assert.False(t, cod.IsOnlinePayment)
}

func TestDecodeReserveStock(t *testing.T) {
	res := decodeFixture[ReserveStockResult](t, "reserve_stock.json")
	assert.Equal(t, []int64{77, 78}, res.ReservationIDs)
	assert.NotEmpty(t, res.Message)

	shortfall := decodeFixture[StockShortfall](t, "reserve_stock_shortfall.json")
	assert.NotEmpty(t, shortfall.Detail)
	require.Len(t, shortfall.FailedItems, 1)
	assert.Equal(t, int64(5), shortfall.FailedItems[0].ProductID)
	assert.Equal(t, 1, shortfall.FailedItems[0].Available)
	assert.Equal(t, 3, shortfall.FailedItems[0].Requested)
}

func TestDecodeShippingOptions(t *testing.T) {
	opts := decodeFixture[[]ShippingOption](t, "shipping_options.json")

	require.Len(t, opts, 3)
	assert.Equal(t, "acs", opts[0].ProviderCode)
	assert.Equal(t, "home_delivery", opts[0].Kind)
	assert.Equal(t, "EUR", opts[0].Currency)
	assert.Equal(t, "boxnow", opts[2].ProviderCode)
	assert.NotEmpty(t, opts[0].Metadata)
}

func TestDecodeFreeShippingInfo(t *testing.T) {
	info := decodeFixture[FreeShippingInfo](t, "free_shipping_info.json")
	assert.Equal(t, "30.0", info.MinThreshold.String())
	assert.Equal(t, "EUR", info.Currency)
	require.Len(t, info.Providers, 3)
}

func TestDecodeAcsStations(t *testing.T) {
	stations := decodeFixture[[]AcsStation](t, "acs_stations_nearest.json")
	require.NotEmpty(t, stations)
	s := stations[0]
	assert.Equal(t, "AAT", s.ExternalID)
	assert.Equal(t, "502", s.BranchCode)
	assert.Equal(t, 8, s.ShopKind)
	assert.Equal(t, "10434", s.PostalCode)
	assert.NotEmpty(t, s.Lat)
	assert.True(t, s.IsActive)
}

func TestDecodeBoxNowLocker(t *testing.T) {
	l := decodeFixture[BoxNowLocker](t, "boxnow_nearest.json")
	assert.Equal(t, "25", l.ID)
	assert.Equal(t, "apm", l.Type)
	assert.Equal(t, "11472", l.PostalCode)
	assert.NotEmpty(t, l.Distance.String())
}

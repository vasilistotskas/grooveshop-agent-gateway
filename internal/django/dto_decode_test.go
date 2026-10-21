package django

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests decode fixtures recorded from the real dev API, so they prove
// the hand-written DTOs decode a real response. They do not prove Django
// still sends a field: that is the consumer contract's job
// (TestDjangoConsumerContract in contract_test.go).

func decodeFixture[T any](t *testing.T, name string) T {
	t.Helper()
	var out T
	dec := json.NewDecoder(bytes.NewReader(fixture(t, name)))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&out))
	return out
}

func TestDecodeProductDetail(t *testing.T) {
	p := decodeFixture[ProductDetail](t, "product_detail.json")

	assert.Equal(t, int64(1), p.ID)
	assert.Equal(t, "including-hope", p.Slug)
	assert.Equal(t, int64(262), p.Category)
	assert.Equal(t, "464.68", p.EffectivePrice.String())
	assert.Empty(t, p.PriorPrice, "null prior price: nothing announced")
	assert.Empty(t, p.AnnouncedDiscountPercent)
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
	assert.Equal(t, "934.61", hit.EffectivePrice.String())
	assert.Equal(t, "983.8", hit.PriorPrice.String())
	assert.Equal(t, "5", hit.AnnouncedDiscountPercent.String())
	assert.Empty(t, res.Results[1].PriorPrice)
	assert.Equal(t, "el", hit.LanguageCode)
	assert.Equal(t, "product", hit.ContentType)
	assert.Equal(t, 250, hit.Stock)
}

func TestDecodeAgentFavourites(t *testing.T) {
	favs := decodeFixture[[]AgentFavourite](t, "agent_favourites.json")

	require.Len(t, favs, 3)
	announced := favs[0]
	assert.Equal(t, int64(6125), announced.ProductID)
	assert.Equal(t, "428.72", announced.EffectivePrice.String())
	assert.Equal(t, "476.35", announced.PriorPrice.String())
	assert.Equal(t, "10", announced.AnnouncedDiscountPercent.String())
	assert.Equal(t, "EUR", announced.Currency)
	assert.True(t, announced.InStock)
	assert.Equal(t, "17.91", favs[1].EffectivePrice.String())
	assert.Empty(t, favs[1].PriorPrice, "null prior price: nothing announced")
	assert.Empty(t, favs[1].AnnouncedDiscountPercent)
	assert.Empty(t, favs[2].EffectivePrice, "null while not recorded")
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
	assert.Empty(t, c.AppliedPromotions)
	assert.Empty(t, c.AppliedCoupons)
	assert.Equal(t, "0.0", c.TotalDiscountValue.String())
}

func TestDecodeCartWithCoupon(t *testing.T) {
	c := decodeFixture[Cart](t, "cart_with_coupon.json")

	assert.Equal(t, "92.94", c.PromotionDiscount.String())
	assert.False(t, c.PromotionFreeShipping)
	assert.Equal(t, "929.36", c.TotalPrice.String())
	assert.Equal(t, []AppliedPromotion{{
		PromotionID: 7, Name: "Save 10%", Code: "SAVE10",
		Amount: json.Number("92.94"),
	}}, c.AppliedPromotions)
	// A null rejection decodes to "": the code applies.
	assert.Equal(t, []AppliedCoupon{{
		Code: "SAVE10", Amount: json.Number("92.94"),
	}}, c.AppliedCoupons)
}

func TestDecodeCartAppliedCouponRejection(t *testing.T) {
	var c Cart
	dec := json.NewDecoder(strings.NewReader(`{"appliedPromotions":` +
		`[{"promotionId":3,"name":"2+1","code":null,"amount":10.79}],` +
		`"appliedCoupons":[{"code":"GAN20","amount":0,"freeShipping":false,` +
		`"gift":false,"rejection":"discount_code_not_applicable"}]}`))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&c))

	// An automatic promotion's null code decodes to "".
	assert.Empty(t, c.AppliedPromotions[0].Code)
	assert.Equal(t, "discount_code_not_applicable",
		c.AppliedCoupons[0].Rejection)
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

// PayWayByID reads the detail serializer, a superset of the list's.
func TestDecodePayWayDetail(t *testing.T) {
	pw := decodeFixture[PayWay](t, "pay_way_detail.json")

	assert.EqualValues(t, 3, pw.ID)
	assert.Equal(t, "PAY_ON_DELIVERY", pw.Key)
	assert.True(t, pw.Active)
	assert.Equal(t, "cash_on_delivery", pw.ProviderCode)
	assert.Equal(t, SettlementCourierCash, pw.Settlement)
	assert.True(t, pw.IsCollectedLater())
	assert.Equal(t, "0.0", pw.Cost.String())
}

func TestDecodePayWays(t *testing.T) {
	page := decodeFixture[Page[PayWay]](t, "pay_way.json")

	require.Len(t, page.Results, 2)
	viva := page.Results[1]
	assert.Equal(t, "VIVA_WALLET", viva.Key)
	assert.Equal(t, "viva_wallet", viva.ProviderCode)
	assert.Equal(t, SettlementOnline, viva.Settlement)
	assert.True(t, viva.IsOnlineSettlement())
	assert.False(t, viva.IsCollectedLater())
	assert.Equal(t, "1.0", viva.Cost.String())
	cod := page.Results[0]
	assert.Equal(t, "PAY_ON_DELIVERY", cod.Key)
	assert.Equal(t, "cash_on_delivery", cod.ProviderCode)
	assert.Equal(t, SettlementCourierCash, cod.Settlement)
	assert.False(t, cod.IsOnlineSettlement())
	assert.True(t, cod.IsCollectedLater())
}

// The whole reason the booleans are gone. Go cannot tell a JSON member
// that is absent from one that is present and false, so the day Django
// drops the deprecated isOnlinePayment column every pay way would have
// decoded as "offline" and every caller would have believed it: online
// methods advertised to agents as collect-on-delivery instruments,
// card orders placed down the cash-on-delivery path.
//
// A settlement string has no such ambiguity — absent decodes to "",
// which is neither online nor collected-later, so each predicate says
// no and every branch fails closed.
func TestPayWaySettlementFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		payload   string
		online    bool
		collected bool
		known     bool
	}{
		{"online", `{"settlement":"online"}`, true, false, true},
		{"courier cash", `{"settlement":"courier_cash"}`, false, true, true},
		{"locker terminal", `{"settlement":"carrier_terminal"}`,
			false, true, true},
		{"bank transfer", `{"settlement":"offline_transfer"}`,
			false, true, true},
		{"member absent", `{"providerCode":"viva_wallet"}`,
			false, false, false},
		{"member empty", `{"settlement":""}`, false, false, false},
		{"value we do not know", `{"settlement":"crypto"}`,
			false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pw PayWay
			require.NoError(t, json.Unmarshal([]byte(tc.payload), &pw))

			assert.Equal(t, tc.online, pw.IsOnlineSettlement())
			assert.Equal(t, tc.collected, pw.IsCollectedLater())
			assert.Equal(t, tc.known, pw.HasKnownSettlement())
		})
	}
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
	assert.Equal(t, "GR", opts[0].CountryCode)
	assert.Nil(t, opts[0].MaxWeightGrams, "no weight cap on the rate")
	assert.False(t, opts[0].ExceedsMaxWeight)
	assert.False(t, opts[0].ExceedsParcelSize)
	assert.NotEmpty(t, opts[0].Metadata)
}

func TestDecodeFreeShippingInfo(t *testing.T) {
	info := decodeFixture[FreeShippingInfo](t, "free_shipping_info.json")
	assert.Equal(t, "30.0", info.MinThreshold.String())
	assert.Equal(t, "EUR", info.Currency)
	assert.Equal(t, "GR", info.CountryCode)
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

package checkout

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func pricingTenant() *tenant.Tenant {
	return &tenant.Tenant{
		SchemaName:      "public",
		DefaultLocale:   "el",
		DefaultCurrency: "EUR",
		PrimaryDomain:   "shop.example.test",
		Domain:          "shop.example.test",
		// A store that takes discount codes.
		Features: django.StoreFeatures{Promotions: true},
	}
}

// pricingCart renders a one-line cart (2 × 100.00 EUR) with the given
// promotion state, in the upstream wire shape. Its freeThresholdValue is
// Django's: the items after the promotion discount, never below zero.
func pricingCart(promo string, freeShipping bool) string {
	rawCodes := []byte("[]")
	discount, err := money.MinorUnits(promo)
	if err != nil {
		panic(err)
	}
	threshold := money.Format(max(20000-discount, 0))
	return `{"id":1,"uuid":"29eb4495-e018-45e7-b59c-6646302bd4ef",` +
		`"items":[{"id":10,"product":{"id":5,"translations":` +
		`{"el":{"name":"Δοκιμαστικό","description":""}},` +
		`"finalPrice":100.00},"quantity":2,"finalPrice":100.00,` +
		`"totalPrice":200.00}],"totalPrice":200.00,` +
		`"totalDiscountValue":12.00,"promotionDiscount":` + promo + `,` +
		`"freeThresholdValue":` + threshold + `,` +
		`"promotionFreeShipping":` + strconv.FormatBool(freeShipping) + `,` +
		`"appliedCouponCodes":` + string(rawCodes) + `,` +
		`"totalItems":2,"totalItemsUnique":1,"totalWeightGrams":107,` +
		`"currency":"EUR"}`
}

// pricingSessionCart is the cart every pricing test session names.
const pricingSessionCart = "29eb4495-e018-45e7-b59c-6646302bd4ef"

// quoteDjango serves the pricing cart, answers each quote with quote(r)
// (the status and body Django would), and records every quote request.
func quoteDjango(
	t *testing.T, cart string, quote func(django.QuoteRequest) (int, string),
) (*django.Client, *[]django.QuoteRequest) {
	t.Helper()
	var asked []django.QuoteRequest
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cart",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(cart))
		})
	mux.HandleFunc("POST /api/v1/cart/quote",
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, pricingSessionCart, r.Header.Get("X-Cart-Id"))
			var req django.QuoteRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			asked = append(asked, req)
			status, body := quote(req)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		})
	mux.HandleFunc("GET /api/v1/pay_way/{id}",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":7,"key":"PAY_ON_DELIVERY",` +
				`"translations":{},"active":true,"cost":2.00,` +
				`"freeThreshold":0,"providerCode":"cash_on_delivery",` +
				`"settlement":"courier_cash"}`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	log := slog.New(slog.DiscardHandler)
	return django.New(srv.URL+"/api/v1", "api.example.test", "secret",
		5*time.Second, log, obs.NewMetrics()), &asked
}

// quoteDigest is the cart digest of a quote that prices a delivery.
const quoteDigest = "5e1f0c2a9b3d4e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f"

// quoteBody is a quote of the pricing cart (200.00 of items) with a
// promotion of 20.00, and the rows given; "null" leaves one out. As
// Django's, it has a cart digest when it prices a delivery.
func quoteBody(shipping, fee, expected string) string {
	digest := `"` + quoteDigest + `"`
	if shipping == "null" {
		digest = "null"
	}
	return quoteBodyDigest(shipping, fee, expected, digest)
}

// quoteBodyDigest is quoteBody with the cart digest given.
func quoteBodyDigest(shipping, fee, expected, digest string) string {
	return `{"itemsSubtotal":200.00,"discountTotal":20.00,` +
		`"appliedPromotions":[{"promotionId":3,"name":"-20","code":"SAVE20",` +
		`"amount":20.00}],"blockingCoupon":null,"freeThresholdValue":180.00,` +
		`"shipping":` + shipping + `,"paymentMethodFee":` + fee + `,` +
		`"loyaltyDiscount":0,"giftCardAmount":null,"grandTotal":null,` +
		`"amountDue":null,"currency":"EUR","expectedTotal":` + expected + `,` +
		`"cartDigest":` + digest + `,` +
		`"shippingOptions":[{"providerCode":"acs","providerName":"ACS",` +
		`"kind":"home_delivery","price":3.50,"waivedBy":null,` +
		`"exceedsMaxWeight":false,"exceedsParcelSize":false}],"payWays":[]}`
}

// readySession has chosen a delivery, a pay way and its buyer.
func readySession() *Session {
	s := NewSession("public", "shop.example.test", ProtocolUCP,
		pricingSessionCart)
	s.Fulfillment = Fulfillment{
		Kind: FulfillmentHomeDelivery, ProviderCode: "acs",
		CountryCode: "GR", RegionID: "ATT",
	}
	s.PayWayID = 7
	s.Buyer.Email = "buyer@example.test"
	return s
}

// Every amount is Django's quote of the checkout's own choices, the
// email included: the gateway adds nothing up, and the total is the one
// an order is placed at.
func TestComputePricingIsTheQuote(t *testing.T) {
	dj, asked := quoteDjango(t, pricingCart("20.00", false),
		func(django.QuoteRequest) (int, string) {
			return http.StatusOK, quoteBody(
				`{"price":3.50,"waivedBy":null}`,
				`{"amount":2.00,"waived":false}`,
				`{"amount":18550,"currency":"EUR"}`)
		})

	p, cart, err := ComputePricing(t.Context(), dj, pricingTenant(),
		readySession())
	require.NoError(t, err)
	require.NotNil(t, cart)

	assert.Equal(t, []django.QuoteRequest{{
		PayWayID: 7, ShippingKind: FulfillmentHomeDelivery,
		ShippingProviderCode: "acs", CountryID: "GR", RegionID: "ATT",
		Email: "buyer@example.test",
	}}, *asked)
	assert.Len(t, p.Lines, 1)
	assert.EqualValues(t, 20000, p.ItemsSubtotal)
	assert.EqualValues(t, 2000, p.DiscountTotal)
	assert.Equal(t, []PricedDiscount{
		{PromotionID: 3, Name: "-20", Code: "SAVE20", Amount: 2000},
	}, p.Discounts)
	assert.True(t, p.HasDelivery)
	assert.EqualValues(t, 350, p.DeliveryFee)
	assert.True(t, p.HasPaymentFee)
	assert.EqualValues(t, 200, p.PaymentFee)
	assert.EqualValues(t, 18550, p.Total, "the quote's expected total")
	assert.Equal(t, quoteDigest, p.CartDigest, "and its cart's digest")
	assert.Equal(t, "EUR", p.Currency)
	assert.Len(t, p.ShippingOptions, 1)
	require.NotNil(t, p.PayWay, "the pay way's settlement is read")
	assert.Nil(t, p.DeliveryRefused)
	assert.Nil(t, p.PayWayRefused)
}

// A delivery a promotion waived is a free row; a pay way's waived fee is
// no row.
func TestComputePricingWaivers(t *testing.T) {
	dj, _ := quoteDjango(t, pricingCart("20.00", true),
		func(django.QuoteRequest) (int, string) {
			return http.StatusOK, quoteBody(
				`{"price":0,"waivedBy":"promotion"}`,
				`{"amount":0,"waived":true}`,
				`{"amount":18000,"currency":"EUR"}`)
		})

	p, _, err := ComputePricing(t.Context(), dj, pricingTenant(),
		readySession())
	require.NoError(t, err)
	assert.True(t, p.HasDelivery)
	assert.Zero(t, p.DeliveryFee)
	assert.False(t, p.HasPaymentFee)
	assert.EqualValues(t, 18000, p.Total)
}

// A checkout that has not chosen everything has no total an order is
// placed at: its total is the rows the quote priced, and it is not final.
func TestComputePricingWithoutEveryChoice(t *testing.T) {
	dj, asked := quoteDjango(t, pricingCart("20.00", false),
		func(django.QuoteRequest) (int, string) {
			return http.StatusOK, quoteBody(
				`{"price":3.50,"waivedBy":null}`, "null", "null")
		})
	s := readySession()
	s.PayWayID = 0

	p, _, err := ComputePricing(t.Context(), dj, pricingTenant(), s)
	require.NoError(t, err)
	assert.Zero(t, (*asked)[0].PayWayID)
	assert.False(t, p.HasPaymentFee)
	assert.EqualValues(t, 20000-2000+350, p.Total)
	assert.Equal(t, quoteDigest, p.CartDigest,
		"what the order is made of: the pay way is not part of it")
	assert.Nil(t, p.PayWay)
}

// A pickup checkout's quote names its pickup point, as its order does:
// the quote's cart digest covers the point, so an order sent to it is
// the cart the quote priced.
func TestAPickupQuoteNamesItsPoint(t *testing.T) {
	dj, asked := quoteDjango(t, pricingCart("20.00", false),
		func(django.QuoteRequest) (int, string) {
			return http.StatusOK, quoteBody(
				`{"price":3.50,"waivedBy":null}`,
				`{"amount":2.00,"waived":false}`,
				`{"amount":18550,"currency":"EUR"}`)
		})
	s := readySession()
	s.Fulfillment.Kind = FulfillmentPickupPoint
	s.Fulfillment.ProviderCode = django.ShippingProviderBoxNow
	s.Fulfillment.BoxnowLockerID = "4"

	_, _, err := ComputePricing(t.Context(), dj, pricingTenant(), s)
	require.NoError(t, err)
	require.Len(t, *asked, 1)
	assert.Equal(t, django.PickupPoint{BoxnowLockerID: "4"},
		(*asked)[0].PickupPoint)
	assert.Equal(t, s.Fulfillment.PickupPoint(), (*asked)[0].PickupPoint,
		"the point the order is sent to")
}

// A delivery the store refuses is dropped from the quote whole, its
// pickup point with it: the rest is priced as a checkout without one.
func TestARefusedPickupIsDroppedWithItsPoint(t *testing.T) {
	dj, asked := quoteDjango(t, pricingCart("20.00", false),
		func(r django.QuoteRequest) (int, string) {
			if r.ShippingKind != "" {
				return http.StatusBadRequest, `{"detail":"Too large.",` +
					`"reason":"shipping_parcel_too_large",` +
					`"code":"shipping_parcel_too_large"}`
			}
			return http.StatusOK, quoteBody("null",
				`{"amount":2.00,"waived":false}`, "null")
		})
	s := readySession()
	s.Fulfillment.Kind = FulfillmentPickupPoint
	s.Fulfillment.ProviderCode = django.ShippingProviderBoxNow
	s.Fulfillment.BoxnowLockerID = "4"

	p, _, err := ComputePricing(t.Context(), dj, pricingTenant(), s)
	require.NoError(t, err)
	require.Len(t, *asked, 2)
	assert.Equal(t, django.PickupPoint{BoxnowLockerID: "4"},
		(*asked)[0].PickupPoint)
	assert.Equal(t, django.QuoteRequest{PayWayID: 7, CountryID: "GR",
		Email: "buyer@example.test"}, (*asked)[1],
		"no delivery, no region, no pickup point")
	require.NotNil(t, p.DeliveryRefused)
	assert.Empty(t, p.CartDigest)
}

// A quote that prices a delivery without the digest of its cart leaves
// out what the order is checked against: it is an error, as a quote
// without a row it was asked for is.
func TestAQuoteWithoutItsCartDigestIsAnError(t *testing.T) {
	dj, _ := quoteDjango(t, pricingCart("20.00", false),
		func(django.QuoteRequest) (int, string) {
			return http.StatusOK, quoteBodyDigest(
				`{"price":3.50,"waivedBy":null}`,
				`{"amount":2.00,"waived":false}`,
				`{"amount":18550,"currency":"EUR"}`, "null")
		})

	_, _, err := ComputePricing(t.Context(), dj, pricingTenant(),
		readySession())
	require.ErrorIs(t, err, django.ErrUpstreamDown)

	// A delivery priced without a pay way must carry the digest too: a
	// quote of one without it cannot be read either.
	dj, _ = quoteDjango(t, pricingCart("20.00", false),
		func(django.QuoteRequest) (int, string) {
			return http.StatusOK, quoteBodyDigest(
				`{"price":3.50,"waivedBy":null}`, "null", "null", "null")
		})
	s := readySession()
	s.PayWayID = 0
	_, _, err = ComputePricing(t.Context(), dj, pricingTenant(), s)
	require.ErrorIs(t, err, django.ErrUpstreamDown, "delivery, no pay way")
}

// A choice the store refuses is named and left out, and the rest is
// priced: the pay way, judged against the delivery, then the delivery.
func TestComputePricingDropsARefusedChoice(t *testing.T) {
	dj, asked := quoteDjango(t, pricingCart("0.0", false),
		func(r django.QuoteRequest) (int, string) {
			switch {
			case r.PayWayID != 0:
				return http.StatusBadRequest, `{"detail":"Not offered.",` +
					`"reason":"pay_way_unavailable","code":"pay_way_unavailable"}`
			case r.ShippingKind != "":
				return http.StatusBadRequest, `{"detail":"Too heavy.",` +
					`"reason":"shipping_weight_exceeded",` +
					`"code":"shipping_weight_exceeded"}`
			}
			return http.StatusOK, quoteBody("null", "null", "null")
		})

	p, _, err := ComputePricing(t.Context(), dj, pricingTenant(),
		readySession())
	require.NoError(t, err)
	require.Len(t, *asked, 3)
	assert.Equal(t, "GR", (*asked)[2].CountryID,
		"the options are still listed for the country")
	assert.Equal(t, &Refusal{Code: django.CodePayWayUnavailable,
		Detail: "Not offered."}, p.PayWayRefused)
	assert.Equal(t, &Refusal{Code: django.CodeShippingWeightExceeded,
		Detail: "Too heavy."}, p.DeliveryRefused)
	assert.False(t, p.HasDelivery)
	assert.Nil(t, p.PayWay, "a refused pay way is not the checkout's")
}

// A quote that cannot be read is an error, never a total without the
// row it could not price, and so is a cart Django no longer has.
func TestComputePricingQuoteFailures(t *testing.T) {
	for status, body := range map[int]string{
		http.StatusServiceUnavailable: `{"detail":"down"}`,
		http.StatusNotFound: `{"detail":"Cart not found",` +
			`"code":"cart_not_found"}`,
	} {
		dj, _ := quoteDjango(t, pricingCart("0.0", false),
			func(django.QuoteRequest) (int, string) { return status, body })
		_, _, err := ComputePricing(t.Context(), dj, pricingTenant(),
			readySession())
		require.Error(t, err, status)
	}
}

// The store's refusal of the checkout's other data (an email it does not
// take, say) is named, and only the cart is priced; a pay way the store
// no longer has is a refused choice; an empty cart has nothing to price.
func TestComputePricingRefusedData(t *testing.T) {
	t.Run("the checkout's data", func(t *testing.T) {
		dj, asked := quoteDjango(t, pricingCart("0.0", false),
			func(r django.QuoteRequest) (int, string) {
				if r.Email != "" {
					return http.StatusBadRequest, `{"email":["Enter a ` +
						`valid email address."],"code":"invalid",` +
						`"fields":{"email":[{"message":"Enter a valid ` +
						`email address.","code":"invalid"}]}}`
				}
				return http.StatusOK, quoteBody("null", "null", "null")
			})
		p, _, err := ComputePricing(t.Context(), dj, pricingTenant(),
			readySession())
		require.NoError(t, err)
		require.Len(t, *asked, 2)
		assert.Equal(t, django.QuoteRequest{CountryID: "GR"}, (*asked)[1],
			"the cart is priced, its deliveries still listed")
		require.NotNil(t, p.InputRefused)
		assert.Contains(t, p.InputRefused.Detail, "valid email")
	})
	t.Run("a pay way the store no longer has", func(t *testing.T) {
		dj, _ := quoteDjango(t, pricingCart("0.0", false),
			func(r django.QuoteRequest) (int, string) {
				if r.PayWayID != 0 {
					return http.StatusNotFound, `{"detail":"Payment method ` +
						`not found","code":"pay_way_not_found"}`
				}
				return http.StatusOK, quoteBody(
					`{"price":3.50,"waivedBy":null}`, "null", "null")
			})
		p, _, err := ComputePricing(t.Context(), dj, pricingTenant(),
			readySession())
		require.NoError(t, err)
		require.NotNil(t, p.PayWayRefused)
		assert.Equal(t, django.CodePayWayNotFound, p.PayWayRefused.Code)
		assert.True(t, p.HasDelivery)
	})
	t.Run("an empty cart", func(t *testing.T) {
		dj, _ := quoteDjango(t, pricingCart("0.0", false),
			func(django.QuoteRequest) (int, string) {
				return http.StatusBadRequest, `{"detail":"Cart is empty.",` +
					`"code":"cart_empty"}`
			})
		p, _, err := ComputePricing(t.Context(), dj, pricingTenant(),
			readySession())
		require.NoError(t, err)
		assert.Zero(t, p.Total)
		assert.Equal(t, "EUR", p.Currency)
	})
	t.Run("a quote in another currency than the store's", func(t *testing.T) {
		dj, _ := quoteDjango(t, pricingCart("0.0", false),
			func(django.QuoteRequest) (int, string) {
				return http.StatusOK, strings.Replace(quoteBody("null",
					"null", "null"), `"EUR"`, `"USD"`, 1)
			})
		_, _, err := ComputePricing(t.Context(), dj, pricingTenant(),
			readySession())
		require.Error(t, err, "never shown under the store's currency")
	})
}

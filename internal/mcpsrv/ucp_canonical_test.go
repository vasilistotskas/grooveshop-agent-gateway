package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

func agentMeta(idem string) *MetaIn {
	return &MetaIn{
		UCPAgent:       &UCPAgentIn{Profile: "https://agent.test/p.json"},
		IdempotencyKey: idem,
	}
}

// meta.ucp-agent.profile is what lets a business negotiate capabilities,
// so every operation refuses without it rather than guessing.
func TestMetaValidateRequiresAgentProfile(t *testing.T) {
	for name, m := range map[string]*MetaIn{
		"nil meta":      nil,
		"no agent":      {},
		"empty profile": {UCPAgent: &UCPAgentIn{}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, m.validate(false))
		})
	}

	// The URL itself is vetted where it is fetched (ucp.ProfileResolver).
	require.NoError(t, agentMeta("").validate(false))
}

// complete and cancel place or void an order, so a retry without a key
// could do it twice.
func TestMetaValidateRequiresIdempotencyKeyWhereItMatters(t *testing.T) {
	assert.ErrorContains(t, agentMeta("").validate(true),
		"idempotency-key")
	assert.NoError(t, agentMeta("key-1").validate(true))
}

func TestCheckoutInLines(t *testing.T) {
	in := &UCPCheckoutIn{LineItems: []UCPLineItemReqIn{
		{Item: UCPItemIn{ID: "5"}, Quantity: 2},
		{Item: UCPItemIn{ID: "7"}, Quantity: 1},
		{Item: UCPItemIn{ID: "5"}, Quantity: 3},
	}}
	lines, err := in.lines()
	require.NoError(t, err)
	assert.Equal(t, []checkout.Line{{ProductID: 5, Quantity: 5},
		{ProductID: 7, Quantity: 1}}, lines, "one line per product")

	// One below 1 is refused, never read as 1 or folded into another
	// line of its product: the checkout would hold a quantity the
	// platform never asked for.
	neg := &UCPCheckoutIn{LineItems: []UCPLineItemReqIn{
		{Item: UCPItemIn{ID: "7"}, Quantity: 4},
		{Item: UCPItemIn{ID: "7"}, Quantity: -3},
	}}
	_, err = neg.lines()
	te := codeOf(t, err, mcptool.CodeInvalid)
	assert.Equal(t, "checkout.line_items: product 7 asks for -3: "+
		checkout.ErrInvalidQuantity.Error(), te.Hint)

	// A non-numeric id is a caller error, not an empty checkout.
	bad := &UCPCheckoutIn{LineItems: []UCPLineItemReqIn{
		{Item: UCPItemIn{ID: "not-a-product"}},
	}}
	_, err = bad.lines()
	te = codeOf(t, err, mcptool.CodeInvalid)
	assert.Contains(t, te.Hint, "line_items[0].item.id")
}

// codeOf requires err to be a ToolError with code, and returns it.
func codeOf(t *testing.T, err error, code string) *mcptool.ToolError {
	t.Helper()
	te, ok := errors.AsType[*mcptool.ToolError](err)
	require.True(t, ok, "not a ToolError: %v", err)
	assert.Equal(t, code, te.Code, te.Hint)
	return te
}

// The codes are the discount extension's member, read only while it is
// negotiated; an empty array names none, which clears them.
func TestCheckoutInDiscountCodes(t *testing.T) {
	neg := discountNegotiated(t, true)
	codes, present := (&UCPCheckoutIn{}).discountCodes(neg)
	assert.False(t, present)
	assert.Nil(t, codes)

	codes, present = (&UCPCheckoutIn{
		Discounts: &UCPDiscountsIn{Codes: []string{}},
	}).discountCodes(neg)
	assert.True(t, present)
	assert.Empty(t, codes)

	submitted := &UCPCheckoutIn{
		Discounts: &UCPDiscountsIn{Codes: []string{"SAVE10"}},
	}
	codes, present = submitted.discountCodes(neg)
	assert.True(t, present)
	assert.Equal(t, []string{"SAVE10"}, codes)

	// A member of an extension that was not negotiated is ignored.
	codes, present = submitted.discountCodes(discountNegotiated(t, false))
	assert.False(t, present)
	assert.Nil(t, codes)
}

// Update replaces the checkout: with the extension negotiated, a request
// without codes clears the cart's coupon, whatever this checkout
// submitted (a checkout's codes are its cart's). Without the extension
// nothing is read.
func TestUpdateReplacesTheDiscountCodes(t *testing.T) {
	codes, present := (&UCPCheckoutIn{}).replacedDiscountCodes(
		discountNegotiated(t, true))
	assert.True(t, present)
	assert.Empty(t, codes)

	codes, present = (&UCPCheckoutIn{Discounts: &UCPDiscountsIn{
		Codes: []string{"SAVE10"}}}).replacedDiscountCodes(
		discountNegotiated(t, true))
	assert.True(t, present)
	assert.Equal(t, []string{"SAVE10"}, codes)

	_, present = (&UCPCheckoutIn{}).replacedDiscountCodes(
		discountNegotiated(t, false))
	assert.False(t, present)
}

// Update replaces the checkout, so the buyer is what the request carries.
// The gateway's own fulfillment member is not in the response a platform
// sends back, so one the request omits keeps the stored delivery, and so
// does the spec's fulfillment member, which is not negotiated.
func TestCheckoutInApplyToReplacesTheBuyer(t *testing.T) {
	s := &checkout.Session{
		Buyer:       checkout.Buyer{FirstName: "Μαρία", Email: "m@e.test"},
		Fulfillment: checkout.Fulfillment{Kind: "home_delivery"},
		PayWayID:    3,
	}
	(&UCPCheckoutIn{Buyer: &UCPBuyerReqIn{
		FirstName: "Άννα", PhoneNumber: "+30691",
	}}).applyTo(s)
	assert.Equal(t, checkout.Buyer{FirstName: "Άννα", Phone: "+30691"},
		s.Buyer)
	assert.Equal(t, "home_delivery", s.Fulfillment.Kind)
	assert.EqualValues(t, 3, s.PayWayID)

	var spec UCPCheckoutIn
	require.NoError(t, json.Unmarshal([]byte(`{"fulfillment":{"methods":[`+
		`{"type":"shipping","destinations":[{"postal_code":"10431"}]}]}}`),
		&spec))
	spec.applyTo(s)
	assert.Equal(t, checkout.Buyer{}, s.Buyer)
	assert.Equal(t, "home_delivery", s.Fulfillment.Kind)

	(&UCPCheckoutIn{Fulfillment: &checkout.Fulfillment{
		Kind: "pickup_point", ProviderCode: "boxnow",
	}}).applyTo(s)
	assert.Equal(t, "pickup_point", s.Fulfillment.Kind)
}

// discountNegotiated intersects a store with promotions on with a platform
// that declares checkout, and the discount extension when asked.
func discountNegotiated(t *testing.T, platformDeclares bool) ucp.Negotiated {
	t.Helper()
	caps := `"dev.ucp.shopping.checkout":[{"version":"` + ucp.Version +
		`","schema":"https://ucp.dev/schemas/shopping/checkout.json"}]`
	if platformDeclares {
		caps += `,"` + ucp.CapabilityDiscount + `":[{"version":"` +
			ucp.Version + `","schema":` +
			`"https://ucp.dev/schemas/shopping/discount.json",` +
			`"extends":"dev.ucp.shopping.checkout"}]`
	}
	var p ucp.PlatformProfile
	require.NoError(t, json.Unmarshal([]byte(`{"ucp":{"version":"`+
		ucp.Version+`","capabilities":{`+caps+`}}}`), &p))
	tn := &tenant.Tenant{}
	tn.Features.Promotions = true
	return ucp.Negotiate(ucp.BusinessCapabilities(tn), &p)
}

// payWayDjango serves the recorded pay-way fixture.
func payWayDjango(t *testing.T) *django.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pay_way", func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
			"fixtures", "django", "pay_way.json"))
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.test", "secret",
		5_000_000_000, obs.NewLogger(slog.LevelError, "test", "test"),
		obs.NewMetrics())
}

func payWayTenant() *tenant.Tenant {
	return &tenant.Tenant{
		SchemaName: "public", DefaultLocale: "el",
		Domain: "shop.test",
	}
}

// The instrument type is the protocol's vocabulary and the pay-way id is
// the store's; only the business bridges them.
func TestResolvePayWayMapsAdvertisedInstrument(t *testing.T) {
	id, err := resolvePayWay(context.Background(), payWayDjango(t),
		payWayTenant(), checkout.Fulfillment{}, 0,
		[]UCPInstrumentIn{{
			HandlerID: ucp.HandlerID,
			Type:      ucp.InstrumentCashOnDelivery,
		}})
	require.NoError(t, err)
	assert.Positive(t, id, "cash on delivery must resolve to a pay way")
}

func TestResolvePayWayRejectsWhatTheStoreCannotSettle(t *testing.T) {
	dj, tn := payWayDjango(t), payWayTenant()

	// A type this store never advertised.
	_, err := resolvePayWay(context.Background(), dj, tn,
		checkout.Fulfillment{}, 0, []UCPInstrumentIn{{Type: "card"}})
	assert.Contains(t, codeOf(t, err, mcptool.CodeUnsupported).Hint,
		"not available")

	// A handler that is not ours — the id must match what we advertised.
	_, err = resolvePayWay(context.Background(), dj, tn,
		checkout.Fulfillment{}, 0, []UCPInstrumentIn{{
			HandlerID: "com.stripe.payments",
			Type:      ucp.InstrumentCashOnDelivery,
		}})
	assert.Contains(t, codeOf(t, err, mcptool.CodeInvalid).Hint,
		"unknown payment handler_id")
}

// A pay way whose settlement we cannot read is NOT an instrument.
//
// The profile fails closed on purpose: an ONLINE method needs the
// store's hosted page and nothing here can acquire it, so advertising
// one as agent-payable would let a platform "complete" a checkout that
// no one can charge. The predecessor of this filter tested "not
// online", which an ABSENT settlement satisfies — and absent is exactly
// what an upstream field removal produces, since Go decodes a missing
// member to the zero value with no way to tell it apart from a real
// one. This serves a payload with the member missing and with a value
// this gateway does not know; both must withhold the instrument.
func TestResolvePayWayWithholdsUnreadableSettlements(t *testing.T) {
	for _, tc := range []struct{ name, settlement string }{
		{"member absent", ""},
		{"value we do not know", `"settlement":"crypto",`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"count":1,"results":[{"id":1,` + tc.settlement +
				`"active":true,"providerCode":"cash_on_delivery",` +
				`"cost":"0","freeThreshold":"0","sortOrder":1,` +
				`"translations":{}}]}`
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/pay_way",
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			dj := django.New(srv.URL+"/api/v1", "api.test", "secret",
				5_000_000_000, obs.NewLogger(slog.LevelError, "test", "test"),
				obs.NewMetrics())

			_, err := resolvePayWay(context.Background(), dj,
				payWayTenant(), checkout.Fulfillment{}, 0,
				[]UCPInstrumentIn{{
					HandlerID: ucp.HandlerID,
					Type:      ucp.InstrumentCashOnDelivery,
				}})

			assert.ErrorContains(t, err, "not available")
		})
	}
}

// When the platform marks a selection, that is the one to settle — not
// whichever happens to be first.
func TestResolvePayWayHonoursTheSelectedInstrument(t *testing.T) {
	id, err := resolvePayWay(context.Background(), payWayDjango(t),
		payWayTenant(), checkout.Fulfillment{}, 0,
		[]UCPInstrumentIn{
			{Type: "some_unmodelled_type"},
			{Type: ucp.InstrumentCashOnDelivery, Selected: true},
		})
	require.NoError(t, err)
	assert.Positive(t, id)
}

// carrierPayWays serves a store with two cash-on-delivery methods, 5 for
// ACS and 7 for BOX NOW, and an online one, 9, for ACS only: the list is
// filtered by the delivery's carrier, as Django filters it. Any other
// pay way is a 404.
func carrierPayWays(t *testing.T) *django.Client {
	t.Helper()
	payWay := func(id, provider, settlement string) string {
		return `{"id":` + id + `,"active":true,"providerCode":"` +
			provider + `","settlement":"` + settlement + `","cost":"0",` +
			`"freeThreshold":"0","sortOrder":` + id + `,"translations":{}}`
	}
	ways := map[string]string{
		"5": payWay("5", django.ProviderCashOnDelivery,
			django.SettlementCourierCash),
		"7": payWay("7", django.ProviderCashOnDelivery,
			django.SettlementCourierCash),
		"9": payWay("9", "viva_wallet", django.SettlementOnline),
	}
	offered := map[string][]string{
		"":                            {"5", "7", "9"},
		django.ShippingProviderACS:    {"5", "9"},
		django.ShippingProviderBoxNow: {"7"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pay_way",
		func(w http.ResponseWriter, r *http.Request) {
			var results []string
			for _, id := range offered[r.URL.Query().Get(
				"shippingProviderCode")] {
				results = append(results, ways[id])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":` +
				strconv.Itoa(len(results)) + `,"results":[` +
				strings.Join(results, ",") + `]}`))
		})
	mux.HandleFunc("GET /api/v1/pay_way/{id}",
		func(w http.ResponseWriter, r *http.Request) {
			body, ok := ways[r.PathValue("id")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.test", "secret",
		5_000_000_000, obs.NewLogger(slog.LevelError, "test", "test"),
		obs.NewMetrics())
}

// deliveryBy is a complete delivery by the carrier.
func deliveryBy(provider string) checkout.Fulfillment {
	f := checkout.Fulfillment{
		Kind: checkout.FulfillmentHomeDelivery, ProviderCode: provider,
		CountryCode: "GR", City: "Athens", Zipcode: "10431",
		Street: "Patision", StreetNumber: "1",
	}
	if provider == django.ShippingProviderBoxNow {
		f.Kind = checkout.FulfillmentPickupPoint
		f.BoxnowLockerID = "42"
	}
	return f
}

// A store may settle one instrument with several methods. The one the
// checkout holds is kept when it settles the instrument, so a complete
// naming the instrument the checkout holds is not a change of method.
func TestResolvePayWayKeepsTheHeldMethodOfTheInstrument(t *testing.T) {
	cod := []UCPInstrumentIn{{Type: ucp.InstrumentCashOnDelivery}}
	for _, tc := range []struct {
		name     string
		delivery checkout.Fulfillment
		held     int64
		want     int64
	}{
		{"none held: the first", checkout.Fulfillment{}, 0, 5},
		{"the held one settles it", checkout.Fulfillment{}, 7, 7},
		{"the held one is online", checkout.Fulfillment{}, 9, 5},
		{"the delivery does not offer the held one",
			deliveryBy(django.ShippingProviderBoxNow), 5, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := resolvePayWay(context.Background(),
				carrierPayWays(t), payWayTenant(), tc.delivery, tc.held,
				cod)
			require.NoError(t, err)
			assert.Equal(t, tc.want, id)
		})
	}
}

// An update that names no method keeps the held one only while the
// delivery is offered it: otherwise the method of the same instrument
// the delivery is offered, or none.
func TestPayWayForDeliveryFollowsTheDelivery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delivery checkout.Fulfillment
		held     int64
		want     int64
	}{
		{"nothing held", deliveryBy(django.ShippingProviderBoxNow), 0, 0},
		{"offered for the delivery",
			deliveryBy(django.ShippingProviderACS), 5, 5},
		{"another carrier's method of the instrument",
			deliveryBy(django.ShippingProviderBoxNow), 5, 7},
		{"an online method the delivery is not offered",
			deliveryBy(django.ShippingProviderBoxNow), 9, 0},
		{"a method the store no longer has",
			deliveryBy(django.ShippingProviderACS), 404, 0},
		{"no delivery yet: every method is offered",
			checkout.Fulfillment{}, 7, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &handlers{deps: Deps{Django: carrierPayWays(t)}}
			s := &checkout.Session{
				Fulfillment: tc.delivery, PayWayID: tc.held,
			}
			require.NoError(t, h.payWayForDelivery(
				context.Background(), payWayTenant(), s))
			assert.Equal(t, tc.want, s.PayWayID)
		})
	}
}

func TestNamesPayment(t *testing.T) {
	assert.False(t, (&UCPPaymentChoice{}).namesPayment())
	assert.False(t, (&UCPPaymentChoice{
		Payment: &UCPPaymentIn{}}).namesPayment(), "payment: {}")
	assert.True(t, (&UCPPaymentChoice{PayWayID: 2}).namesPayment())
	assert.True(t, (&UCPPaymentChoice{Payment: &UCPPaymentIn{
		Instruments: []UCPInstrumentIn{{Type: "x"}}}}).namesPayment())
}

func gatedTenant(hosted bool) *tenant.Tenant {
	return &tenant.Tenant{
		AgentCommerceEnabled:      true,
		AgentHostedPaymentEnabled: hosted,
	}
}

// negotiatedFor intersects a tenant's capabilities with a platform that
// declares checkout, plus the hosted-selection extension when asked.
func negotiatedFor(
	t *testing.T, tn *tenant.Tenant, platformDeclaresHosted bool,
) ucp.Negotiated {
	t.Helper()
	caps := `"dev.ucp.shopping.checkout":[{"version":"` + ucp.Version +
		`","schema":"https://ucp.dev/schemas/shopping/checkout.json"}]`
	if platformDeclaresHosted {
		caps += `,"` + ucp.HostedSelectionCapability + `":[{"version":"` +
			ucp.HandlerVersion + `","schema":` +
			`"https://payments.grooveshop.space/hosted_selection.json",` +
			`"extends":"dev.ucp.shopping.checkout"}]`
	}
	var p ucp.PlatformProfile
	require.NoError(t, json.Unmarshal([]byte(`{"ucp":{"version":"`+
		ucp.Version+`","capabilities":{`+caps+`}}}`), &p))
	return ucp.Negotiate(ucp.BusinessCapabilities(tn), &p)
}

// A submitted pay-way id is REFUSED, not ignored, unless the extension is
// negotiated — the store's gate on AND the platform declaring it: a silent
// no-op would complete against whatever was selected before, so the buyer
// could be charged a different way than the agent chose.
func TestApplyHostedSelectionRefusesUnlessNegotiated(t *testing.T) {
	for name, neg := range map[string]ucp.Negotiated{
		"store gate off":            negotiatedFor(t, gatedTenant(false), true),
		"platform does not declare": negotiatedFor(t, gatedTenant(true), false),
	} {
		t.Run(name, func(t *testing.T) {
			s := &checkout.Session{PayWayID: 7}
			err := (&UCPPaymentChoice{PayWayID: 2}).applyHostedSelection(s, neg)
			assert.Contains(t, codeOf(t, err, mcptool.CodeInvalid).Hint,
				"pay_way_id is not accepted")
			assert.EqualValues(t, 7, s.PayWayID,
				"the prior selection must stand")
		})
	}
}

// pay_way_id and instruments select competing methods: a call sending
// both is refused, even where the extension is negotiated.
func TestApplyHostedSelectionRefusesBothSelections(t *testing.T) {
	s := &checkout.Session{PayWayID: 7}
	err := (&UCPPaymentChoice{PayWayID: 2, Payment: &UCPPaymentIn{
		Instruments: []UCPInstrumentIn{{
			Type: ucp.InstrumentCashOnDelivery}},
	}}).applyHostedSelection(s, negotiatedFor(t, gatedTenant(true), true))
	assert.Contains(t, codeOf(t, err, mcptool.CodeInvalid).Hint,
		"not both")
	assert.EqualValues(t, 7, s.PayWayID, "the prior selection must stand")
}

func TestApplyHostedSelectionHonoursWhenNegotiated(t *testing.T) {
	s := &checkout.Session{PayWayID: 7}
	require.NoError(t, (&UCPPaymentChoice{PayWayID: 2}).applyHostedSelection(
		s, negotiatedFor(t, gatedTenant(true), true)))
	assert.EqualValues(t, 2, s.PayWayID)
}

// A caller that never mentions the member is unaffected — the common case
// for a canonical platform, which uses instruments.
func TestApplyHostedSelectionIgnoresAbsentMember(t *testing.T) {
	for _, hosted := range []bool{true, false} {
		s := &checkout.Session{PayWayID: 7}
		require.NoError(t, (&UCPPaymentChoice{}).applyHostedSelection(
			s, negotiatedFor(t, gatedTenant(hosted), hosted)))
		assert.EqualValues(t, 7, s.PayWayID)
	}
}

// The store sells per item: a line asserting another sale basis is
// refused, never reinterpreted; C62 or none is a count of items.
func TestCheckoutInLinesRefusesAnotherSaleBasis(t *testing.T) {
	line := func(unit string) *UCPCheckoutIn {
		return &UCPCheckoutIn{LineItems: []UCPLineItemReqIn{{Item: UCPItemIn{
			ID: "5", QuantityUnit: &UCPQuantityUnitIn{Unit: unit}}, Quantity: 2}}}
	}
	_, err := line("KGM").lines()
	assert.Contains(t, codeOf(t, err, mcptool.CodeInvalid).Hint,
		"quantity_unit \"KGM\" is not this item's sale basis")
	lines, err := line("C62").lines()
	require.NoError(t, err)
	assert.Equal(t, []checkout.Line{{ProductID: 5, Quantity: 2}}, lines)
}

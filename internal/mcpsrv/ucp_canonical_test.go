package mcpsrv

import (
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
	require.ErrorContains(t, agentMeta("").validate(true),
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

// Update replaces the checkout, so the buyer is what the request carries,
// and with the fulfillment extension negotiated so is the delivery: its
// shipping destination and the option its group selects. Without the
// extension the member is ignored and the stored delivery kept.
func TestCheckoutInApplyToReplacesTheBuyerAndTheDelivery(t *testing.T) {
	held := checkout.Fulfillment{
		Kind: checkout.FulfillmentHomeDelivery, ProviderCode: "acs",
		CountryCode: "GR", City: "Αθήνα", Zipcode: "10431",
		Street: "Ερμού 12", RegionID: "GR-14",
	}
	session := func() *checkout.Session {
		return &checkout.Session{
			Buyer:       checkout.Buyer{FirstName: "Μαρία", Email: "m@e.test"},
			Fulfillment: held, PayWayID: 3,
		}
	}
	decode := func(t *testing.T, raw string) *UCPCheckoutIn {
		t.Helper()
		var in UCPCheckoutIn
		require.NoError(t, json.Unmarshal([]byte(raw), &in))
		return &in
	}
	written := `{"buyer":{"first_name":"Άννα","phone_number":"+30691"},` +
		`"fulfillment":{"methods":[{"id":"shipping","type":"shipping",` +
		`"line_item_ids":["410"],"selected_destination_id":"home",` +
		`"destinations":[{"id":"home","street_address":" Πατησίων 5 ",` +
		`"extended_address":"2ος όροφος","address_locality":"Αθήνα",` +
		`"address_region":"Αττική","postal_code":"10434",` +
		`"address_country":"GR","first_name":"not read"}],` +
		`"groups":[{"id":"parcel","selected_option_id":"elta"}]}]}}`

	t.Run("not negotiated: the stored delivery is kept", func(t *testing.T) {
		s := session()
		delivered, err := decode(t, written).applyTo(s,
			fulfillmentNegotiated(t, false))
		require.NoError(t, err)
		assert.False(t, delivered)
		assert.Equal(t, checkout.Buyer{FirstName: "Άννα", Phone: "+30691"},
			s.Buyer)
		assert.Equal(t, held, s.Fulfillment)
		assert.EqualValues(t, 3, s.PayWayID)
	})

	neg := fulfillmentNegotiated(t, true)
	for name, tc := range map[string]struct {
		raw  string
		want checkout.Fulfillment
	}{
		"a destination and an option": {written, checkout.Fulfillment{
			Kind: checkout.FulfillmentHomeDelivery, ProviderCode: "elta",
			CountryCode: "GR", City: "Αθήνα", Zipcode: "10434",
			Street: "Πατησίων 5", AddressLine2: "2ος όροφος",
			RegionID: "Αττική",
		}},
		"omitted: no delivery": {`{}`, checkout.Fulfillment{}},
		"no option selected": {`{"fulfillment":{"methods":[{"type":` +
			`"shipping","destinations":[{"street_address":"Πατησίων 5",` +
			`"address_locality":"Αθήνα","postal_code":"10434",` +
			`"address_country":"GR"}],"groups":[{"id":"parcel",` +
			`"selected_option_id":null}]}]}}`, checkout.Fulfillment{
			CountryCode: "GR", City: "Αθήνα", Zipcode: "10434",
			Street: "Πατησίων 5",
		}},
		"a destination written without an id, selected by the id it " +
			"is given": {`{"fulfillment":{"methods":[{"type":"shipping",` +
			`"selected_destination_id":"shipping_address","destinations":` +
			`[{"street_address":"Πατησίων 5","address_locality":"Αθήνα",` +
			`"postal_code":"10434","address_country":"GR"}]}]}}`,
			checkout.Fulfillment{CountryCode: "GR", City: "Αθήνα",
				Zipcode: "10434", Street: "Πατησίων 5"}},
		"the destination given, by its id": {`{"fulfillment":{"methods":` +
			`[{"type":"shipping","selected_destination_id":` +
			`"shipping_address","groups":[{"id":"parcel",` +
			`"selected_option_id":"elta"}]}]}}`, withCarrier(held, "elta")},
		"the destination given, no option": {`{"fulfillment":{"methods":` +
			`[{"type":"shipping","selected_destination_id":` +
			`"shipping_address"}]}}`, withCarrier(held, "")},
		// An update may name the method by the id the business gave it
		// instead of its type.
		"the method named by its id": {`{"fulfillment":{"methods":` +
			`[{"id":"shipping","line_item_ids":["410"],` +
			`"selected_destination_id":"shipping_address","groups":` +
			`[{"id":"parcel","selected_option_id":"elta"}]}]}}`,
			withCarrier(held, "elta")},
	} {
		t.Run(name, func(t *testing.T) {
			s := session()
			delivered, err := decode(t, tc.raw).applyTo(s, neg)
			require.NoError(t, err)
			assert.True(t, delivered)
			assert.Equal(t, tc.want, s.Fulfillment)
		})
	}

	for name, raw := range map[string]string{
		"a pickup method": `{"fulfillment":{"methods":[{"type":"pickup",` +
			`"selected_destination_id":"locker-4"}]}}`,
		"two shipping methods": `{"fulfillment":{"methods":[` +
			`{"type":"shipping"},{"type":"shipping"}]}}`,
		"two destinations": `{"fulfillment":{"methods":[{"type":` +
			`"shipping","destinations":[{"postal_code":"1"},` +
			`{"postal_code":"2"}]}]}}`,
		"a destination id it never gave": `{"fulfillment":{"methods":` +
			`[{"type":"shipping","selected_destination_id":"saved-7"}]}}`,
		"another destination than the one written": `{"fulfillment":` +
			`{"methods":[{"type":"shipping","selected_destination_id":` +
			`"a","destinations":[{"id":"b","postal_code":"1"}]}]}}`,
		"another destination than the one written without an id": `{` +
			`"fulfillment":{"methods":[{"type":"shipping",` +
			`"selected_destination_id":"saved-7","destinations":` +
			`[{"postal_code":"1"}]}]}}`,
		"two groups": `{"fulfillment":{"methods":[{"type":"shipping",` +
			`"groups":[{"id":"parcel"},{"id":"other"}]}]}}`,
		"a method id it never gave": `{"fulfillment":{"methods":` +
			`[{"id":"express","line_item_ids":["410"]}]}}`,
		"a method named by neither": `{"fulfillment":{"methods":` +
			`[{"line_item_ids":["410"]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := session()
			_, err := decode(t, raw).applyTo(s, neg)
			te, ok := errors.AsType[*mcptool.ToolError](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, mcptool.CodeInvalid, te.Code)
			assert.Equal(t, session(), s, "nothing applied")
		})
	}
}

// withCarrier is f delivered home by carrier, or with no carrier chosen.
func withCarrier(f checkout.Fulfillment, carrier string) checkout.Fulfillment {
	f.Kind, f.ProviderCode = "", ""
	if carrier != "" {
		f.Kind, f.ProviderCode = checkout.FulfillmentHomeDelivery, carrier
	}
	return f
}

// A delivery to another address than the one whose street line the store
// refused clears the refusal; the same address keeps it.
func TestCheckoutInApplyToClearsARefusedStreetOnlyForANewAddress(
	t *testing.T,
) {
	neg := fulfillmentNegotiated(t, true)
	line := func(street string) *UCPCheckoutIn {
		return &UCPCheckoutIn{Fulfillment: &UCPFulfillmentIn{
			Methods: []UCPFulfillmentMethodIn{{Type: "shipping",
				Destinations: []UCPShippingDestinationIn{{
					StreetAddress: street, AddressLocality: "Αθήνα",
					PostalCode: "10431", AddressCountry: "GR",
				}}}}}}
	}
	s := &checkout.Session{}
	_, err := line("Ερμού").applyTo(s, neg)
	require.NoError(t, err)
	s.StreetRefused = &checkout.Refusal{Code: "street_number_missing"}

	_, err = line("Ερμού").applyTo(s, neg)
	require.NoError(t, err)
	assert.NotNil(t, s.StreetRefused)
	_, err = line("Ερμού 12").applyTo(s, neg)
	require.NoError(t, err)
	assert.Nil(t, s.StreetRefused)
}

// fulfillmentNegotiated intersects a store with a platform that declares
// checkout, and the fulfillment extension when asked.
func fulfillmentNegotiated(t *testing.T, platformDeclares bool) ucp.Negotiated {
	t.Helper()
	caps := `"dev.ucp.shopping.checkout":[{"version":"` + ucp.Version +
		`","schema":"https://ucp.dev/schemas/shopping/checkout.json"}]`
	if platformDeclares {
		caps += `,"` + ucp.CapabilityFulfillment + `":[{"version":"` +
			ucp.Version + `","schema":` +
			`"https://ucp.dev/schemas/shopping/fulfillment.json",` +
			`"extends":"dev.ucp.shopping.checkout"}]`
	}
	var p ucp.PlatformProfile
	require.NoError(t, json.Unmarshal([]byte(`{"ucp":{"version":"`+
		ucp.Version+`","capabilities":{`+caps+`}}}`), &p))
	return ucp.Negotiate(ucp.BusinessCapabilities(&tenant.Tenant{}), &p)
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
		5_000_000_000, slog.New(slog.DiscardHandler),
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
	id, err := resolvePayWay(t.Context(), payWayDjango(t),
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
	_, err := resolvePayWay(t.Context(), dj, tn,
		checkout.Fulfillment{}, 0, []UCPInstrumentIn{{Type: "card"}})
	assert.Contains(t, codeOf(t, err, mcptool.CodeUnsupported).Hint,
		"not available")

	// A handler that is not ours — the id must match what we advertised.
	_, err = resolvePayWay(t.Context(), dj, tn,
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
				5_000_000_000, slog.New(slog.DiscardHandler),
				obs.NewMetrics())

			_, err := resolvePayWay(t.Context(), dj,
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
	id, err := resolvePayWay(t.Context(), payWayDjango(t),
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
		"":                         {"5", "7", "9"},
		django.ShippingProviderACS: {"5", "9"},
		"elta":                     {"7"},
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
		5_000_000_000, slog.New(slog.DiscardHandler),
		obs.NewMetrics())
}

// deliveryBy is a complete delivery by the carrier.
func deliveryBy(provider string) checkout.Fulfillment {
	f := checkout.Fulfillment{
		Kind: checkout.FulfillmentHomeDelivery, ProviderCode: provider,
		CountryCode: "GR", City: "Athens", Zipcode: "10431",
		Street: "Patision 1",
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
			deliveryBy("elta"), 5, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := resolvePayWay(t.Context(),
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
		{"nothing held", deliveryBy("elta"), 0, 0},
		{"offered for the delivery",
			deliveryBy(django.ShippingProviderACS), 5, 5},
		{"another carrier's method of the instrument",
			deliveryBy("elta"), 5, 7},
		{"an online method the delivery is not offered",
			deliveryBy("elta"), 9, 0},
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
				t.Context(), payWayTenant(), s))
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

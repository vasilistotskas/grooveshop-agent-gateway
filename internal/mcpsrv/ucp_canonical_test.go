package mcpsrv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/checkout"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
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

	// A non-https profile cannot be fetched safely.
	http := &MetaIn{UCPAgent: &UCPAgentIn{Profile: "http://agent.test/p"}}
	assert.ErrorContains(t, http.validate(false), "https")

	require.NoError(t, agentMeta("").validate(false))
}

// complete and cancel place or void an order, so a retry without a key
// could do it twice.
func TestMetaValidateRequiresIdempotencyKeyWhereItMatters(t *testing.T) {
	assert.ErrorContains(t, agentMeta("").validate(true),
		"idempotency-key")
	assert.NoError(t, agentMeta("key-1").validate(true))
}

func TestCheckoutInProductQuantities(t *testing.T) {
	in := &UCPCheckoutIn{LineItems: []UCPLineItemReqIn{
		{Item: UCPItemIn{ID: "5"}, Quantity: 2},
		// Quantity is optional on the wire; one unit is the sane read.
		{Item: UCPItemIn{ID: "7"}},
	}}
	lines, err := in.productQuantities()
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Equal(t, productQuantity{ProductID: 5, Quantity: 2}, lines[0])
	assert.Equal(t, productQuantity{ProductID: 7, Quantity: 1}, lines[1])

	// A non-numeric id is a caller error, not an empty checkout.
	bad := &UCPCheckoutIn{LineItems: []UCPLineItemReqIn{
		{Item: UCPItemIn{ID: "not-a-product"}},
	}}
	_, err = bad.productQuantities()
	assert.ErrorContains(t, err, "line_items[0].item.id")
}

// Absent means "leave the coupon alone" and an empty array means "remove
// it" — collapsing the two would make removal impossible.
func TestCheckoutInDiscountCodes(t *testing.T) {
	codes, present := (&UCPCheckoutIn{}).discountCodes()
	assert.False(t, present)
	assert.Nil(t, codes)

	codes, present = (&UCPCheckoutIn{
		Discounts: &UCPDiscountsIn{},
	}).discountCodes()
	assert.True(t, present)
	assert.Empty(t, codes)

	codes, present = (&UCPCheckoutIn{
		Discounts: &UCPDiscountsIn{Codes: []string{"SAVE10"}},
	}).discountCodes()
	assert.True(t, present)
	assert.Equal(t, []string{"SAVE10"}, codes)
}

// A platform updating one field must not wipe the others.
func TestCheckoutInApplyToLeavesAbsentMembersAlone(t *testing.T) {
	s := &checkout.Session{
		Buyer:       checkout.Buyer{FirstName: "Μαρία", Email: "m@e.test"},
		Fulfillment: checkout.Fulfillment{Kind: "home_delivery"},
		PayWayID:    3,
	}
	(&UCPCheckoutIn{}).applyTo(s)

	assert.Equal(t, "Μαρία", s.Buyer.FirstName)
	assert.Equal(t, "home_delivery", s.Fulfillment.Kind)
	assert.EqualValues(t, 3, s.PayWayID)

	(&UCPCheckoutIn{Buyer: &UCPBuyerReqIn{
		FirstName: "Άννα", PhoneNumber: "+30691",
	}}).applyTo(s)
	assert.Equal(t, "Άννα", s.Buyer.FirstName)
	assert.Equal(t, "+30691", s.Buyer.Phone)
}

// payWayDjango serves the recorded pay-way fixture.
func payWayDjango(t *testing.T) *django.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pay_way", func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata",
			"fixtures", "django", "pay_way.json"))
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return django.New(srv.URL+"/api/v1", "api.test", "secret",
		5_000_000_000, obs.NewLogger("error", "test", "test"),
		obs.NewMetrics())
}

func payWayTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName: "public", DefaultLocale: "el",
		},
		Domain: "shop.test",
	}
}

// The instrument type is the protocol's vocabulary and the pay-way id is
// the store's; only the business bridges them.
func TestResolvePayWayMapsAdvertisedInstrument(t *testing.T) {
	id, err := resolvePayWay(context.Background(), payWayDjango(t),
		payWayTenant(), &UCPPaymentIn{Instruments: []UCPInstrumentIn{{
			HandlerID: ucp.HandlerID,
			Type:      ucp.InstrumentCashOnDelivery,
		}}})
	require.NoError(t, err)
	assert.Positive(t, id, "cash on delivery must resolve to a pay way")
}

func TestResolvePayWayRejectsWhatTheStoreCannotSettle(t *testing.T) {
	dj, tn := payWayDjango(t), payWayTenant()

	// Nothing submitted: completing would place an order with no way to
	// pay for it.
	_, err := resolvePayWay(context.Background(), dj, tn, nil)
	assert.ErrorContains(t, err, "payment.instruments is required")

	// A type this store never advertised.
	_, err = resolvePayWay(context.Background(), dj, tn,
		&UCPPaymentIn{Instruments: []UCPInstrumentIn{{Type: "card"}}})
	assert.ErrorContains(t, err, "not available")

	// A handler that is not ours — the id must match what we advertised.
	_, err = resolvePayWay(context.Background(), dj, tn,
		&UCPPaymentIn{Instruments: []UCPInstrumentIn{{
			HandlerID: "com.stripe.payments",
			Type:      ucp.InstrumentCashOnDelivery,
		}}})
	assert.ErrorContains(t, err, "unknown payment handler_id")
}

// When the platform marks a selection, that is the one to settle — not
// whichever happens to be first.
func TestResolvePayWayHonoursTheSelectedInstrument(t *testing.T) {
	id, err := resolvePayWay(context.Background(), payWayDjango(t),
		payWayTenant(), &UCPPaymentIn{Instruments: []UCPInstrumentIn{
			{Type: "some_unmodelled_type"},
			{Type: ucp.InstrumentCashOnDelivery, Selected: true},
		}})
	require.NoError(t, err)
	assert.Positive(t, id)
}

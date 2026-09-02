package ucp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func tenantWith(codes ...string) *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			AgentPaymentInstruments: codes,
		},
	}
}

func TestPaymentHandlersAdvertisesOfflineInstruments(t *testing.T) {
	handlers := paymentHandlers(tenantWith("cash_on_delivery"), "production")

	entries := handlers[HandlerName]
	require.Len(t, entries, 1, "one handler instance per tenant")
	h := entries[0]

	assert.Equal(t, HandlerID, h.ID)
	assert.Equal(t, HandlerVersion, h.Version)
	assert.Equal(t, map[string]any{"environment": "production"}, h.Config)

	require.Len(t, h.AvailableInstruments, 1)
	assert.Equal(t, InstrumentCashOnDelivery, h.AvailableInstruments[0].Type)

	// The declared provider code is what the business derives, so it
	// belongs in constraints rather than on the wire.
	assert.Equal(t, map[string]any{
		"properties": map[string]any{
			"provider_code": map[string]any{"const": "cash_on_delivery"},
		},
	}, h.AvailableInstruments[0].Constraints)
}

// A store with nothing an agent can settle must advertise no handler.
// The registry stays present because UCP requires it even when empty;
// checkout then escalates to the browser instead of claiming a payment
// path that does not exist.
func TestPaymentHandlersEmptyWithoutOfflinePayWays(t *testing.T) {
	for name, tn := range map[string]*tenant.Tenant{
		"no pay ways":     tenantWith(),
		"nil slice":       {},
		"only unmodelled": tenantWith("viva_wallet", "some_new_psp"),
	} {
		t.Run(name, func(t *testing.T) {
			handlers := paymentHandlers(tn, "production")
			require.NotNil(t, handlers, "registry is required even when empty")
			assert.Empty(t, handlers)
		})
	}
}

// Django orders the codes by the merchant's own pay-way ordering, which
// UCP reads as preferred presentation, so the advertised order must
// survive verbatim — and a code repeated across pay-ways must not
// produce a duplicate instrument.
func TestPaymentHandlersPreservesOrderAndDedupes(t *testing.T) {
	tn := tenantWith("cash_on_delivery", "viva_wallet", "cash_on_delivery")
	instruments := paymentHandlers(tn, "production")[HandlerName][0].
		AvailableInstruments

	require.Len(t, instruments, 1)
	assert.Equal(t, InstrumentCashOnDelivery, instruments[0].Type)
}

// The handler's own documents must sit under its versioned base on
// payments.grooveshop.space. That host's reversed labels equal
// HandlerName, which is the exact-match case of the spec's authority
// binding — a platform rejects the entity outright if the schema origin
// is not name-aligned, so a stray host silently disables payment.
func TestHandlerDocumentsAreAuthorityBound(t *testing.T) {
	h := paymentHandlers(tenantWith("cash_on_delivery"),
		"production")[HandlerName][0]

	for field, url := range map[string]string{"spec": h.Spec, "schema": h.Schema} {
		require.NotEmpty(t, url, field)
		assert.True(t, strings.HasPrefix(url, handlerBase+"/"),
			"%s = %q must sit under %s", field, url, handlerBase)
	}

	// Reversing the schema host must reproduce the handler name exactly.
	host, _, found := strings.Cut(
		strings.TrimPrefix(handlerBase, "https://"), "/")
	require.True(t, found, "handlerBase must carry a path")
	labels := strings.Split(host, ".")
	for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
		labels[i], labels[j] = labels[j], labels[i]
	}
	assert.Equal(t, HandlerName, strings.Join(labels, "."),
		"schema host must reverse to the handler name")
}

func hostedTenant(commerce, hosted bool) *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			AgentCommerceEnabled:      commerce,
			AgentHostedPaymentEnabled: hosted,
		},
	}
}

// The extension is advertised only while BOTH tiers allow it. A store
// with the gate off must not advertise a member the business would then
// refuse — a platform that negotiated it would build a call that fails.
func TestHostedSelectionRespectsBothGateTiers(t *testing.T) {
	on := HostedSelection(hostedTenant(true, true))
	require.Len(t, on, 1)
	cap := on[HostedSelectionCapability][0]
	assert.Equal(t, "dev.ucp.shopping.checkout", cap.Extends)
	assert.Equal(t, handlerBase+"/hosted_selection.json", cap.Schema)

	for name, tn := range map[string]*tenant.Tenant{
		"merchant or platform tier off": hostedTenant(true, false),
		"agent commerce off":            hostedTenant(false, true),
		// A payload that never mentioned the gate decodes as off.
		"gate absent": {},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Empty(t, HostedSelection(tn))
		})
	}
}

// The extension schema must be authority-bound to the same host as the
// handler: its name extends space.grooveshop.payments, whose reversed
// host is payments.grooveshop.space.
func TestHostedSelectionCapabilityIsAuthorityBound(t *testing.T) {
	assert.True(t, strings.HasPrefix(
		HostedSelectionCapability, "space.grooveshop.payments."),
		"the name must sit under the handler's namespace or the schema "+
			"host stops matching it")
}

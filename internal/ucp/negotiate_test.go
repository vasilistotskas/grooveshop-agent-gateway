package ucp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The spec's own authority-binding table (overview, Derivation algorithm),
// plus the parser traps it calls out.
func TestAuthorityBindingMatchesTheSpecTable(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		want       bool
	}{
		{"dev.ucp.shopping.checkout", "ucp.dev", true},
		{"dev.ucp.shopping.checkout", "shopping.ucp.dev", true},
		{"com.example.payments.installments", "example.com", true},
		{"com.example.pay", "pay.example.com", true},
		{"com.example.pay", "example.com", true},
		{"com.example.pay", "evil.example", false},
		{"dev.ucp.shopping.checkout", "evil.example", false},
		{"com.examplecorp.pay", "example.com", false},
		{"com.example.pay", "cdn.example.com", false},
	} {
		assert.Equal(t, tc.want,
			authorityBound(tc.name, "https://"+tc.host+"/schema.json"),
			"%s from %s", tc.name, tc.host)
	}
	for _, schema := range []string{
		"https://ucp.dev@evil.example/x.json", // userinfo: host is evil
		"http://ucp.dev/x.json",               // not https
		"https://203.0.113.10/x.json",         // IP literal
		"https://localhost/x.json",            // single label
		"://nope",
	} {
		assert.False(t, authorityBound("dev.ucp.shopping.checkout", schema),
			schema)
	}
	assert.True(t, authorityBound("dev.ucp.shopping.checkout",
		"https://UCP.dev./schemas/shopping/checkout.json"),
		"hosts normalize: lowercase, trailing dot")
}

func platformProfile(t *testing.T, raw string) *PlatformProfile {
	t.Helper()
	var p PlatformProfile
	require.NoError(t, json.Unmarshal([]byte(raw), &p))
	return &p
}

func businessWithExtension() map[string][]Capability {
	return map[string][]Capability{
		CapabilityCheckout: {
			{Version: "2026-04-08"}, {Version: "2026-08-25"},
		},
		CapabilityOrder: {{Version: "2026-08-25"}},
		HostedSelectionCapability: {{
			Version: HandlerVersion, Extends: CapabilityCheckout,
		}},
	}
}

func TestNegotiatePicksTheHighestMutualVersion(t *testing.T) {
	p := platformProfile(t, `{"ucp":{"version":"2026-08-25","capabilities":{
		"dev.ucp.shopping.checkout":[
			{"version":"2026-01-23","schema":"https://ucp.dev/a.json"},
			{"version":"2026-04-08","schema":"https://ucp.dev/a.json"}]}}}`)
	n := Negotiate(businessWithExtension(), p)
	assert.Equal(t, map[string][]CapabilityRef{
		CapabilityCheckout: {{Version: "2026-04-08"}},
	}, n.Declaration(CapabilityCheckout))
	assert.False(t, n.Has(CapabilityOrder), "not declared by the platform")
}

// An extension without its parent in the intersection is pruned, and a
// capability declared from a namespace its schema host does not own is
// treated as absent.
func TestNegotiatePrunesOrphansAndUnboundEntries(t *testing.T) {
	p := platformProfile(t, `{"ucp":{"version":"2026-08-25","capabilities":{
		"dev.ucp.shopping.checkout":[
			{"version":"2026-08-25","schema":"https://evil.example/c.json"}],
		"space.grooveshop.payments.hosted_selection":[
			{"version":"`+HandlerVersion+`",
			 "schema":"https://payments.grooveshop.space/h.json",
			 "extends":"dev.ucp.shopping.checkout"}]}}}`)
	n := Negotiate(businessWithExtension(), p)
	assert.False(t, n.Has(CapabilityCheckout), "unbound schema host")
	assert.False(t, n.Has(HostedSelectionCapability), "orphaned extension")
}

// The response declares the operation's root and its extensions only, and
// the order capability carries the platform's webhook endpoint.
func TestNegotiatedDeclarationAndWebhook(t *testing.T) {
	p := platformProfile(t, `{"ucp":{"version":"2026-08-25","capabilities":{
		"dev.ucp.shopping.checkout":[
			{"version":"2026-08-25","schema":"https://ucp.dev/c.json"}],
		"dev.ucp.shopping.order":[
			{"version":"2026-08-25","schema":"https://ucp.dev/o.json",
			 "config":{"webhook_url":"https://platform.example/hooks"}}],
		"space.grooveshop.payments.hosted_selection":[
			{"version":"`+HandlerVersion+`",
			 "schema":"https://payments.grooveshop.space/h.json",
			 "extends":["dev.ucp.shopping.checkout"]}]}}}`)
	n := Negotiate(businessWithExtension(), p)
	assert.Equal(t, map[string][]CapabilityRef{
		CapabilityCheckout:        {{Version: "2026-08-25"}},
		HostedSelectionCapability: {{Version: HandlerVersion}},
	}, n.Declaration(CapabilityCheckout))
	assert.Equal(t, map[string][]CapabilityRef{
		CapabilityOrder: {{Version: "2026-08-25"}},
	}, n.Declaration(CapabilityOrder))
	assert.Equal(t, "https://platform.example/hooks", n.OrderWebhookURL())
}

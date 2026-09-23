package ucp

import (
	"encoding/json"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/idna"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Root capability names this business implements.
const (
	CapabilityCheckout = "dev.ucp.shopping.checkout"
	CapabilityOrder    = "dev.ucp.shopping.order"
)

// PlatformProfile is the part of a platform's UCP profile negotiation
// reads. The full document is validated against the spec's
// platform_schema before it is decoded into this.
type PlatformProfile struct {
	UCP struct {
		Version      string                          `json:"version"`
		Capabilities map[string][]PlatformCapability `json:"capabilities"`
	} `json:"ucp"`
}

// PlatformCapability is one capability entry a platform declares.
type PlatformCapability struct {
	Version string          `json:"version"`
	Schema  string          `json:"schema"`
	Extends extendsList     `json:"extends"`
	Config  json.RawMessage `json:"config"`
}

// extendsList decodes the spec's `extends`: one parent name, or several.
type extendsList []string

func (e *extendsList) UnmarshalJSON(raw []byte) error {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		*e = extendsList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return err
	}
	*e = many
	return nil
}

// BusinessCapabilities is what this business declares for a tenant — the
// same registry its /.well-known/ucp profile publishes.
func BusinessCapabilities(t *tenant.Tenant) map[string][]Capability {
	caps := map[string][]Capability{
		CapabilityCheckout: {{
			Version: Version,
			Spec:    specBase + "/specification/shopping/checkout/",
			Schema:  specBase + "/schemas/shopping/checkout.json",
		}},
		CapabilityOrder: {{
			Version: Version,
			Spec:    specBase + "/specification/shopping/order/",
			Schema:  specBase + "/schemas/shopping/order.json",
		}},
	}
	// The hosted-payment extension is a per-tenant capability: a store
	// with the gate off must not declare a member it would then refuse.
	maps.Copy(caps, HostedSelection(t))
	return caps
}

// Negotiated is the capability intersection for one request: each
// capability both parties support, at the highest mutual version.
type Negotiated struct {
	caps map[string]negotiatedCapability
}

type negotiatedCapability struct {
	version string
	extends []string
	// config is the platform's config for this capability, e.g. the
	// order capability's webhook_url.
	config json.RawMessage
}

// Negotiate computes the intersection per the spec's algorithm: for each
// business capability the platform also declares, select the highest
// version both list, excluding capabilities with none; then prune
// extensions until each has a parent in the result. Platform entries
// that fail authority binding are treated as absent.
func Negotiate(
	business map[string][]Capability, p *PlatformProfile,
) Negotiated {
	n := Negotiated{caps: map[string]negotiatedCapability{}}
	for name, offered := range business {
		var best negotiatedCapability
		for _, pc := range p.UCP.Capabilities[name] {
			if !authorityBound(name, pc.Schema) {
				continue
			}
			for _, bc := range offered {
				if bc.Version != pc.Version || pc.Version <= best.version {
					continue
				}
				// YYYY-MM-DD compares chronologically as a string.
				best = negotiatedCapability{
					version: pc.Version, config: pc.Config,
				}
				if bc.Extends != "" {
					best.extends = []string{bc.Extends}
				}
			}
		}
		if best.version != "" {
			n.caps[name] = best
		}
	}
	for pruned := true; pruned; {
		pruned = false
		for name, c := range n.caps {
			if len(c.extends) > 0 && !slices.ContainsFunc(c.extends, n.Has) {
				delete(n.caps, name)
				pruned = true
			}
		}
	}
	return n
}

// Has reports whether a capability is active for this request.
func (n Negotiated) Has(name string) bool {
	_, ok := n.caps[name]
	return ok
}

// CapabilityRef names an active capability in a response envelope.
type CapabilityRef struct {
	Version string `json:"version"`
}

// Declaration is the response's `ucp.capabilities`: the root capability
// the operation belongs to and the negotiated extensions of it — never
// capabilities of other operations.
func (n Negotiated) Declaration(root string) map[string][]CapabilityRef {
	out := map[string][]CapabilityRef{}
	for name, c := range n.caps {
		if name == root || slices.Contains(c.extends, root) {
			out[name] = []CapabilityRef{{Version: c.version}}
		}
	}
	return out
}

// OrderCapabilities is the declaration an order webhook carries. A
// webhook exists only for a checkout whose platform negotiated the order
// capability, and this business implements a single version of it.
func OrderCapabilities() map[string][]CapabilityRef {
	return map[string][]CapabilityRef{CapabilityOrder: {{Version: Version}}}
}

// OrderWebhookURL is the platform's order-event endpoint, from the
// negotiated order capability's config; empty when order is not active.
func (n Negotiated) OrderWebhookURL() string {
	c, ok := n.caps[CapabilityOrder]
	if !ok || len(c.config) == 0 {
		return ""
	}
	var config struct {
		WebhookURL string `json:"webhook_url"`
	}
	if json.Unmarshal(c.config, &config) != nil {
		return ""
	}
	return config.WebhookURL
}

// authorityBound applies the spec's authority binding: the schema URL's
// host, reversed label by label, must equal the entity name or be a
// label-aligned prefix of it. It establishes provenance only — whoever
// serves the schema controls the namespace.
func authorityBound(name, schemaURL string) bool {
	u, err := url.Parse(schemaURL)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if _, err := netip.ParseAddr(host); err == nil {
		return false // IP literals are not authorities
	}
	host, err = idna.Lookup.ToASCII(host)
	if err != nil {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	slices.Reverse(labels)
	prefix := strings.Join(labels, ".")
	return name == prefix || strings.HasPrefix(name, prefix+".")
}

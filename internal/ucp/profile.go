package ucp

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// specBase is the version-namespaced root of the published UCP spec.
// ucp.dev serves schemas and service bindings only under a release path;
// the unversioned https://ucp.dev/schemas/... form is a 404, and a
// platform that cannot fetch a declared schema drops the entity. The host
// must stay ucp.dev: under the spec's authority binding a platform rejects
// any dev.ucp.* entity whose schema origin is not name-aligned, so these
// documents are never self-hosted or proxied.
const specBase = "https://ucp.dev/" + Version

// Profile is the /.well-known/ucp business profile document.
type Profile struct {
	UCP  ProfileEnvelope     `json:"ucp"`
	Keys []map[string]string `json:"keys,omitempty"`
}

type ProfileEnvelope struct {
	Version         string                      `json:"version"`
	Services        map[string][]Service        `json:"services"`
	Capabilities    map[string][]Capability     `json:"capabilities"`
	PaymentHandlers map[string][]PaymentHandler `json:"payment_handlers"`
}

type Service struct {
	Version string `json:"version"`
	// Spec and Schema are optional for a business profile — only platform
	// profiles must carry them — but the spec's own reference profile
	// publishes both, and Schema is the OpenRPC binding a platform reads to
	// learn this transport's tool surface.
	Spec      string `json:"spec,omitempty"`
	Transport string `json:"transport"`
	Endpoint  string `json:"endpoint"`
	Schema    string `json:"schema,omitempty"`
}

type Capability struct {
	Version string `json:"version"`
	// Spec is the human-readable specification page; optional but published
	// so agents can link out during negotiation.
	Spec string `json:"spec,omitempty"`
	// Schema is the capability's composable JSON Schema URL — required in
	// business profiles so platforms can fetch it during negotiation.
	Schema  string `json:"schema"`
	Extends string `json:"extends,omitempty"`
}

// BuildProfile renders the per-tenant business profile. The MCP service
// endpoint and every capability live on the tenant's own domain.
//
// The /schemas and /services paths are stable across releases, so they are
// derived from specBase. The per-capability specification pages are not:
// they moved under /specification/shopping/ in 2026-08-25, so each
// capability names its own page and a Version bump must revisit these
// two literals.
func BuildProfile(
	t *tenant.Tenant, key *SigningKey, env string,
) *Profile {
	base := "https://" + t.Domain
	return &Profile{
		UCP: ProfileEnvelope{
			Version: Version,
			Services: map[string][]Service{
				"dev.ucp.shopping": {{
					Version:   Version,
					Spec:      specBase + "/specification/overview/",
					Transport: "mcp",
					Endpoint:  base + "/mcp",
					// The OpenRPC document defining the MCP tool
					// surface. Declaring it asserts a machine-checkable
					// contract, so it may only appear while every method
					// it defines for an ADVERTISED capability exists —
					// the five checkout tools and get_order. The cart
					// and catalog methods it also describes belong to
					// capabilities this profile does not advertise, so a
					// platform negotiating capabilities never calls
					// them.
					Schema: specBase +
						"/services/shopping/mcp.openrpc.json",
				}},
			},
			Capabilities: map[string][]Capability{
				"dev.ucp.shopping.checkout": {{
					Version: Version,
					Spec:    specBase + "/specification/shopping/checkout/",
					Schema:  specBase + "/schemas/shopping/checkout.json",
				}},
				"dev.ucp.shopping.order": {{
					Version: Version,
					Spec:    specBase + "/specification/shopping/order/",
					Schema:  specBase + "/schemas/shopping/order.json",
				}},
			},
			PaymentHandlers: paymentHandlers(t, env),
		},
		Keys: []map[string]string{key.JWK()},
	}
}

// ProfileHandler serves GET /.well-known/ucp. The spec mandates HTTPS
// (Traefik terminates TLS) and a public Cache-Control of at least 60s.
// Each tenant's profile publishes only that tenant's own signing key.
func ProfileHandler(keys *Keys, env string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := tenant.FromContext(r.Context())
		if !ok {
			http.Error(w, "unknown store", http.StatusNotFound)
			return
		}
		key, err := keys.ForSchema(r.Context(), t.SchemaName)
		if err != nil {
			http.Error(w, "profile temporarily unavailable",
				http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		if err := json.NewEncoder(w).Encode(BuildProfile(t, key, env)); err != nil {
			// Headers are already written; nothing recoverable remains.
			_ = err
		}
	})
}

// String renders a profile for diagnostics.
func (p *Profile) String() string {
	raw, _ := json.Marshal(p)
	return fmt.Sprintf("ucp-profile:%s", raw)
}

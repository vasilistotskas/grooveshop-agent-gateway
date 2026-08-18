package ucp

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

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
	Version   string `json:"version"`
	Transport string `json:"transport"`
	Endpoint  string `json:"endpoint"`
}

type Capability struct {
	Version string `json:"version"`
	// Schema is the capability's composable JSON Schema URL — required in
	// business profiles so platforms can fetch it during negotiation.
	Schema  string `json:"schema"`
	Extends string `json:"extends,omitempty"`
}

// BuildProfile renders the per-tenant business profile. The MCP service
// endpoint and every capability live on the tenant's own domain.
func BuildProfile(t *tenant.Tenant, key *SigningKey) *Profile {
	base := "https://" + t.Domain
	return &Profile{
		UCP: ProfileEnvelope{
			Version: Version,
			Services: map[string][]Service{
				"dev.ucp.shopping": {{
					Version:   Version,
					Transport: "mcp",
					Endpoint:  base + "/mcp",
				}},
			},
			Capabilities: map[string][]Capability{
				"dev.ucp.shopping.checkout": {{
					Version: Version,
					Schema:  "https://ucp.dev/schemas/shopping/checkout.json",
				}},
				"dev.ucp.shopping.order": {{
					Version: Version,
					Schema:  "https://ucp.dev/schemas/shopping/order.json",
				}},
			},
			PaymentHandlers: paymentHandlers(t),
		},
		Keys: []map[string]string{key.JWK()},
	}
}

// ProfileHandler serves GET /.well-known/ucp. The spec mandates HTTPS
// (Traefik terminates TLS) and a public Cache-Control of at least 60s.
// Each tenant's profile publishes only that tenant's own signing key.
func ProfileHandler(keys *Keys) http.Handler {
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
		if err := json.NewEncoder(w).Encode(BuildProfile(t, key)); err != nil {
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

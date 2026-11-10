package adminmcp

import (
	"encoding/json"
	"net/http"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// resourceMetadata is /mcp/admin's RFC 9728 document. Its authorization
// server is the platform's staff issuer, never the store's own (which
// signs in the store's customers); MCP clients use the first entry only.
type resourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ScopesSupported        []string `json:"scopes_supported"`
	ResourceName           string   `json:"resource_name"`
}

// ResourceMetadataHandler serves the document at
// /.well-known/oauth-protected-resource/mcp/admin; connectHost is the
// staff issuer's host (config StaffConnectHost).
func ResourceMetadataHandler(connectHost string) http.Handler {
	issuer := storefront.Origin(connectHost)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := tenant.FromContext(r.Context())
		if !ok || !t.Features.StaffAIAccess {
			httpmw.WriteJSONError(w, http.StatusNotFound, "not found")
			return
		}
		doc := resourceMetadata{
			Resource:               Resource(t),
			AuthorizationServers:   []string{issuer},
			BearerMethodsSupported: []string{"header"},
			ScopesSupported:        ReadScopes,
			ResourceName:           identityFor(t).Title,
		}
		w.Header().Set("Content-Type", "application/json")
		// No shared cache, and no longer than the tenant cache: the
		// document must answer 404 soon after the store switches off.
		w.Header().Set("Cache-Control", "private, max-age=300")
		_ = json.NewEncoder(w).Encode(doc)
	})
}

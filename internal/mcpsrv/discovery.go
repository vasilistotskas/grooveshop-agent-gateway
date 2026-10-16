package mcpsrv

import (
	"net/http"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// The store's discovery documents describe one resource, its MCP server,
// in the two forms agents resolve: the ARD manifest
// (https://agenticresourcediscovery.org/spec/, v0.91), and the AI Catalog
// (https://github.com/Agent-Card/ai-catalog) through which MCP Server
// Card discovery (SEP-2127) goes. Both carry the same entry, storeEntry,
// so they cannot disagree about what they describe.
const (
	// aiCatalogSpecVersion is the AI Catalog version both documents
	// follow: an ARD entry is a well-formed catalog entry (ARD §4).
	aiCatalogSpecVersion = "1.0"
	aiCatalogMediaType   = "application/ai-catalog+json"
	// ARD names no media type for its manifest (v0.91 §5.1).
	ardMediaType = "application/json"
)

type catalog struct {
	SpecVersion string         `json:"specVersion"`
	Host        catalogHost    `json:"host"`
	Entries     []catalogEntry `json:"entries"`
}

// catalogHost identifies the catalog's operator: the merchant.
type catalogHost struct {
	DisplayName string `json:"displayName"`
	Identifier  string `json:"identifier"`
}

// catalogEntry is the MCP server as an ARD entry: ARD requires
// displayName and builds its search index from the rest. The AI Catalog
// would leave name and description to the Server Card, but Lighthouse
// validates /.well-known/ai-catalog.json against the ARD schema, so the
// catalog serves the full entry too; the catalog spec allows both keys
// and has consumers ignore the ones they do not know. Name and
// description are the Server Card's own, so neither drifts from it.
type catalogEntry struct {
	Identifier            string   `json:"identifier"`
	Type                  string   `json:"type"`
	URL                   string   `json:"url"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	Capabilities          []string `json:"capabilities"`
	RepresentativeQueries []string `json:"representativeQueries"`
}

func storeEntry(t *tenant.Tenant) catalogEntry {
	id := IdentityFor(t)
	return catalogEntry{
		Identifier:  "urn:air:" + publisher(t) + ":mcp:store",
		Type:        serverCardMediaType,
		URL:         storefront.MCPServerCard(t.Domain),
		DisplayName: id.Title,
		Description: id.Description(),
		// The tools this store is served: one it switched off is not
		// advertised either.
		Capabilities:          toolNamesFor(t.Features),
		RepresentativeQueries: representativeQueries(id.Title),
	}
}

func storeCatalog(t *tenant.Tenant) any {
	return catalog{
		SpecVersion: aiCatalogSpecVersion,
		Host: catalogHost{
			DisplayName: IdentityFor(t).Title,
			Identifier:  publisher(t),
		},
		Entries: []catalogEntry{storeEntry(t)},
	}
}

// ARDHandler serves GET /.well-known/ard.json, the manifest every ARD
// consumer MUST fetch (§5.1).
func ARDHandler() http.Handler {
	return publicDocument(ardMediaType, storeCatalog)
}

// AICatalogHandler serves GET /.well-known/ai-catalog.json, the AI
// Catalog's well-known location.
func AICatalogHandler() http.Handler {
	return publicDocument(aiCatalogMediaType, storeCatalog)
}

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
// Card discovery (SEP-2127) goes. Both are cut from storeEntry, so they
// cannot disagree about what they describe.
const (
	// aiCatalogSpecVersion is the AI Catalog version both documents
	// follow: an ARD entry is a well-formed catalog entry (ARD §4).
	aiCatalogSpecVersion = "1.0"
	aiCatalogMediaType   = "application/ai-catalog+json"
	// ARD names no media type for its manifest (v0.91 §5.1).
	ardMediaType = "application/json"
)

type catalog[E any] struct {
	SpecVersion string      `json:"specVersion"`
	Host        catalogHost `json:"host"`
	Entries     []E         `json:"entries"`
}

// catalogHost identifies the catalog's operator: the merchant.
type catalogHost struct {
	DisplayName string `json:"displayName"`
	Identifier  string `json:"identifier"`
}

// catalogEntry is the AI Catalog entry for the MCP server. It carries no
// name or description: the catalog spec makes the Server Card it points
// at authoritative for both, and says to omit them to avoid drift.
type catalogEntry struct {
	Identifier string `json:"identifier"`
	Type       string `json:"type"`
	URL        string `json:"url"`
}

// ardEntry adds what ARD requires (displayName) and what its search
// index is built from. Name and description are the Server Card's own.
type ardEntry struct {
	catalogEntry
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	Capabilities          []string `json:"capabilities"`
	RepresentativeQueries []string `json:"representativeQueries"`
}

func storeEntry(t *tenant.Tenant) ardEntry {
	id := IdentityFor(t)
	return ardEntry{
		catalogEntry: catalogEntry{
			Identifier: "urn:air:" + publisher(t) + ":mcp:store",
			Type:       serverCardMediaType,
			URL:        storefront.MCPServerCard(t.Domain),
		},
		DisplayName: id.Title,
		Description: id.Description(),
		// The tools this store is served: one it switched off is not
		// advertised either.
		Capabilities:          toolNamesFor(t.Features),
		RepresentativeQueries: representativeQueries(id.Title),
	}
}

func hostOf(t *tenant.Tenant) catalogHost {
	return catalogHost{DisplayName: IdentityFor(t).Title, Identifier: publisher(t)}
}

// ARDHandler serves GET /.well-known/ard.json, the manifest every ARD
// consumer MUST fetch (§5.1).
func ARDHandler() http.Handler {
	return publicDocument(ardMediaType, func(t *tenant.Tenant) any {
		return catalog[ardEntry]{
			SpecVersion: aiCatalogSpecVersion,
			Host:        hostOf(t),
			Entries:     []ardEntry{storeEntry(t)},
		}
	})
}

// AICatalogHandler serves GET /.well-known/ai-catalog.json, the AI
// Catalog's well-known location.
func AICatalogHandler() http.Handler {
	return publicDocument(aiCatalogMediaType, func(t *tenant.Tenant) any {
		return catalog[catalogEntry]{
			SpecVersion: aiCatalogSpecVersion,
			Host:        hostOf(t),
			Entries:     []catalogEntry{storeEntry(t).catalogEntry},
		}
	})
}

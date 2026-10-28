package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// ardSchemaFile is spec/schemas/ard-entry.schema.json (ARD v0.91)
// vendored verbatim from
// https://github.com/ards-project/ard-spec/blob/49eb4df6f3841ccecc5c31d5175117337b568bdf/spec/schemas/ard-entry.schema.json
// Lighthouse's ARD audit validates against the same ArdManifest and
// ArdEntry definitions.
var ardSchemaFile = filepath.Join("..", "..", "testdata", "schemas", "ard",
	"ard-entry.schema.json")

// idnTenant has an internationalised primary domain and a store name
// long enough that the description must be cut.
func idnTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName: "alpha",
			StoreName: "Κατάστημα Φυτών και Βοτάνων της Αλφα με Παράδοση " +
				"Τριών Γενεών",
			PrimaryDomain: "Φυτά.example.test",
		},
		Domain: "www.alpha.example.test",
	}
}

func serve(
	t *testing.T, h http.Handler, tn *tenant.Tenant, method string,
	header http.Header,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/", nil)
	for k, v := range header {
		req.Header[k] = v
	}
	if tn != nil {
		req = req.WithContext(tenant.NewContext(req.Context(), tn))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	return doc
}

func firstEntry(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	entries, ok := doc["entries"].([]any)
	require.True(t, ok, "entries must be an array")
	require.Len(t, entries, 1)
	entry, ok := entries[0].(map[string]any)
	require.True(t, ok)
	return entry
}

func TestDiscoveryDocuments(t *testing.T) {
	for _, tc := range []struct {
		name        string
		handler     http.Handler
		contentType string
		// want is the whole document, but for the ARD entry's
		// capabilities, which TestARDCapabilitiesAreTheRegisteredTools
		// checks against the live server.
		want string
	}{
		{
			name:        "ard.json",
			handler:     ARDHandler(),
			contentType: "application/json",
			want: `{
				"specVersion": "1.0",
				"host": {
					"displayName": "Demo Store",
					"identifier": "shop.example.test"
				},
				"entries": [{
					"identifier": "urn:air:shop.example.test:mcp:store",
					"type": "application/mcp-server-card+json",
					"url": "https://www.shop.example.test/mcp/server-card",
					"displayName": "Demo Store",
					"description": "Shopping tools for Demo Store: catalog, cart, checkout and order tracking.",
					"representativeQueries": [
						"What does Demo Store sell, and how much does it cost?",
						"Which delivery and parcel-locker pickup options does Demo Store offer near my postal code?",
						"Add this product to my Demo Store cart and give me the checkout link",
						"Where is my Demo Store order, and what is its tracking link?"
					]
				}]
			}`,
		},
		{
			name:        "ai-catalog.json",
			handler:     AICatalogHandler(),
			contentType: "application/ai-catalog+json",
			want: `{
				"specVersion": "1.0",
				"host": {
					"displayName": "Demo Store",
					"identifier": "shop.example.test"
				},
				"entries": [{
					"identifier": "urn:air:shop.example.test:mcp:store",
					"type": "application/mcp-server-card+json",
					"url": "https://www.shop.example.test/mcp/server-card",
					"displayName": "Demo Store",
					"description": "Shopping tools for Demo Store: catalog, cart, checkout and order tracking.",
					"representativeQueries": [
						"What does Demo Store sell, and how much does it cost?",
						"Which delivery and parcel-locker pickup options does Demo Store offer near my postal code?",
						"Add this product to my Demo Store cart and give me the checkout link",
						"Where is my Demo Store order, and what is its tracking link?"
					]
				}]
			}`,
		},
	} {
		t.Run(tc.name+"/document", func(t *testing.T) {
			rec := serve(t, tc.handler, cardTenant(), http.MethodGet, nil)
			assert.Equal(t, tc.contentType, rec.Header().Get("Content-Type"))
			doc := decode(t, rec)
			delete(firstEntry(t, doc), "capabilities")
			got, err := json.Marshal(doc)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(got))
		})

		t.Run(tc.name+"/caching and CORS", func(t *testing.T) {
			first := serve(t, tc.handler, cardTenant(), http.MethodGet, nil)
			etag := first.Header().Get("ETag")
			require.NotEmpty(t, etag)
			assert.Equal(t, "public, max-age=3600",
				first.Header().Get("Cache-Control"))
			assert.Equal(t, "*",
				first.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, "ETag",
				first.Header().Get("Access-Control-Expose-Headers"))

			again := serve(t, tc.handler, cardTenant(), http.MethodGet,
				http.Header{"If-None-Match": {etag}})
			assert.Equal(t, http.StatusNotModified, again.Code)
			assert.Empty(t, again.Body.String())

			preflight := serve(t, tc.handler, nil, http.MethodOptions, nil)
			assert.Equal(t, http.StatusNoContent, preflight.Code)
			assert.Equal(t, "*",
				preflight.Header().Get("Access-Control-Allow-Origin"))
		})

		t.Run(tc.name+"/no store", func(t *testing.T) {
			rec := serve(t, tc.handler, nil, http.MethodGet, nil)
			assert.Equal(t, http.StatusNotFound, rec.Code)
		})
	}
}

// ARD requires a name the AI Catalog leaves to the Server Card, so the
// ARD entry restates the card's — and must never drift from it.
func TestARDEntryNamesWhatTheServerCardReports(t *testing.T) {
	for _, tn := range []*tenant.Tenant{cardTenant(), idnTenant()} {
		card := decode(t, serve(t, ServerCardHandler("1.18.0", storefront.NewSite("el")), tn,
			http.MethodGet, nil))
		entry := firstEntry(t, decode(t, serve(t, ARDHandler(), tn,
			http.MethodGet, nil)))
		assert.Equal(t, card["title"], entry["displayName"])
		assert.Equal(t, card["description"], entry["description"])
	}
}

// The entry advertises the tools an agent will find in tools/list, for a
// store with every feature on and for one with none.
func TestARDCapabilitiesAreTheRegisteredTools(t *testing.T) {
	for name, features := range map[string]django.StoreFeatures{
		"every feature": EveryFeature,
		"no feature":    {},
	} {
		t.Run(name, func(t *testing.T) {
			tn := cardTenant()
			tn.Features = features
			ctx := context.Background()
			serverT, clientT := mcp.NewInMemoryTransports()
			_, err := NewServer(Deps{Version: "test"}, IdentityFor(tn), tn.Features).
				Connect(ctx, serverT, nil)
			require.NoError(t, err)
			session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
				Connect(ctx, clientT, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = session.Close() })
			listed, err := session.ListTools(ctx, nil)
			require.NoError(t, err)
			var served []any
			for _, tool := range listed.Tools {
				served = append(served, tool.Name)
			}
			require.NotEmpty(t, served)

			entry := firstEntry(t, decode(t, serve(t, ARDHandler(), tn,
				http.MethodGet, nil)))
			assert.ElementsMatch(t, served, entry["capabilities"])
		})
	}
}

// Both documents: Lighthouse validates /.well-known/ai-catalog.json
// against the ARD schema too, and PageSpeed failed the bare catalog
// entry (2026-10-16).
func TestDiscoveryDocumentsConformToARD(t *testing.T) {
	manifest, entry := compileARDSchema(t)
	for name, handler := range map[string]http.Handler{
		"ard.json":        ARDHandler(),
		"ai-catalog.json": AICatalogHandler(),
	} {
		for _, tn := range []*tenant.Tenant{cardTenant(), idnTenant()} {
			body := serve(t, handler, tn, http.MethodGet, nil).Body.String()

			doc, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
			require.NoError(t, err)
			require.NoError(t, manifest.Validate(doc), name)
			for _, e := range doc.(map[string]any)["entries"].([]any) {
				require.NoError(t, entry.Validate(e), name)
			}

			errs, warnings := ardConformance(t, body)
			assert.Empty(t, errs, name+" "+tn.SchemaName)
			assert.Empty(t, warnings, name+" "+tn.SchemaName)
			queries := firstEntry(t, decode(t, serve(t, handler, tn,
				http.MethodGet, nil)))["representativeQueries"]
			assert.GreaterOrEqual(t, len(queries.([]any)), 2)
			assert.LessOrEqual(t, len(queries.([]any)), 5)
		}
	}
}

// The checks have teeth: the catalog the gateway served before, an
// entry that left its name to the Server Card, is exactly what
// PageSpeed rejected.
func TestARDChecksRejectTheBareCatalogEntry(t *testing.T) {
	manifest, _ := compileARDSchema(t)
	body := `{"specVersion":"1.0",` +
		`"host":{"displayName":"Demo Store","identifier":"shop.example.test"},` +
		`"entries":[{"identifier":"urn:air:shop.example.test:mcp:store",` +
		`"type":"application/mcp-server-card+json",` +
		`"url":"https://www.shop.example.test/mcp/server-card"}]}`

	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	require.NoError(t, err)
	err = manifest.Validate(doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "displayName")

	errs, warnings := ardConformance(t, body)
	assert.Equal(t, []string{"Missing required 'displayName'."}, errs)
	assert.Equal(t, []string{"No 'representativeQueries'."}, warnings)
}

func compileARDSchema(t *testing.T) (manifest, entry *jsonschema.Schema) {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	manifest, err := c.Compile(ardSchemaFile + "#/$defs/ArdManifest")
	require.NoError(t, err)
	entry, err = c.Compile(ardSchemaFile + "#/$defs/ArdEntry")
	require.NoError(t, err)
	return manifest, entry
}

// The rules Lighthouse's ARD audit applies beyond the schema, ported
// from validate_manifest in GoogleChrome/lighthouse
// third-party/ard/ard.js (itself a port of the ARD conformance test).
// Any error fails the audit; any warning costs it full marks.
var (
	ardURN = regexp.MustCompile(
		`^urn:air:([a-zA-Z0-9.-]+)(?::([a-zA-Z0-9._:-]+))?:([a-zA-Z0-9._-]+)$`)
	ardStandardTypes = []string{
		"application/ai-catalog+json",
		"application/agent-card+json",
		"application/a2a-agent-card+json",
		"application/mcp-server-card+json",
		"application/agent-skills+zip",
		"application/agent-skills+gzip",
		`text/markdown; profile="urn:air:agent-skills"`,
		"application/ai-registry",
		"application/ai-registry+json",
	}
)

func ardConformance(t *testing.T, body string) (errs, warnings []string) {
	t.Helper()
	var doc struct {
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &doc))
	require.NotNil(t, doc.Entries, "missing required 'entries' array")
	for _, e := range doc.Entries {
		id, _ := e["identifier"].(string)
		if !ardURN.MatchString(id) {
			errs = append(errs, fmt.Sprintf("identifier %q", id))
		}
		if name, _ := e["displayName"].(string); name == "" {
			errs = append(errs, "Missing required 'displayName'.")
		}
		if typ, _ := e["type"].(string); !slices.Contains(ardStandardTypes, typ) {
			warnings = append(warnings, fmt.Sprintf("type %q", typ))
		}
		_, hasURL := e["url"]
		_, hasData := e["data"]
		if hasURL == hasData {
			errs = append(errs, "exactly one of 'url' and 'data'")
		}
		switch queries := e["representativeQueries"].(type) {
		case nil:
			warnings = append(warnings, "No 'representativeQueries'.")
		case []any:
			if len(queries) < 2 || len(queries) > 5 {
				warnings = append(warnings, fmt.Sprintf(
					"%d representativeQueries", len(queries)))
			}
			for _, q := range queries {
				if _, ok := q.(string); !ok {
					errs = append(errs, fmt.Sprintf("query %v", q))
				}
			}
		default:
			errs = append(errs, "'representativeQueries' must be an array")
		}
		if trust, ok := e["trustManifest"]; ok {
			m, _ := trust.(map[string]any)
			if id, _ := m["identity"].(string); id == "" {
				errs = append(errs, "'trustManifest' without 'identity'")
			}
		}
	}
	return errs, warnings
}

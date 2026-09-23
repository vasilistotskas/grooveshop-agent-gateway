package mcpsrv

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func cardTenant() *tenant.Tenant {
	return &tenant.Tenant{
		TenantConfig: django.TenantConfig{
			SchemaName:    "demostore",
			StoreName:     "Demo Store",
			PrimaryDomain: "shop.example.test",
			FaviconURL:    "https://assets.example.test/favicon.png",
		},
		Domain: "www.shop.example.test",
	}
}

func getCard(t *testing.T, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/mcp/server-card", nil)
	for k, v := range header {
		req.Header[k] = v
	}
	req = req.WithContext(tenant.NewContext(req.Context(), cardTenant()))
	rec := httptest.NewRecorder()
	ServerCardHandler("1.18.0").ServeHTTP(rec, req)
	return rec
}

// The card must validate against the Server Card extension's own schema
// (vendored from modelcontextprotocol/experimental-ext-server-card).
func TestServerCardMatchesTheExtensionSchema(t *testing.T) {
	c := jsonschema.NewCompiler()
	schema, err := c.Compile(filepath.Join("..", "..", "testdata",
		"schemas", "mcp", "server-card.schema.json") + "#/$defs/ServerCard")
	require.NoError(t, err)

	rec := getCard(t, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/mcp-server-card+json",
		rec.Header().Get("Content-Type"))
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(rec.Body.String()))
	require.NoError(t, err)
	require.NoError(t, schema.Validate(doc))

	// No primitives: the extension excludes tools from cards.
	assert.NotContains(t, rec.Body.String(), `"tools"`)
	// The endpoint follows the host the card was fetched on.
	assert.Contains(t, rec.Body.String(),
		`"url":"https://www.shop.example.test/mcp"`)
}

// The card's identity must not contradict what initialize reports.
func TestServerCardIdentityMatchesInitialize(t *testing.T) {
	id := IdentityFor(cardTenant())
	assert.Equal(t, "test.example.shop/store", id.Name,
		"reverse-DNS of the primary domain, whichever host was used")
	assert.Equal(t, "Demo Store", id.Title)
	assert.Contains(t, getCard(t, nil).Body.String(),
		`"name":"test.example.shop/store"`)
}

func TestServerCardCachingAndCORS(t *testing.T) {
	first := getCard(t, nil)
	etag := first.Header().Get("ETag")
	require.NotEmpty(t, etag)
	assert.Equal(t, "*", first.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "ETag", first.Header().Get("Access-Control-Expose-Headers"))
	assert.Equal(t, "public, max-age=3600", first.Header().Get("Cache-Control"))

	again := getCard(t, http.Header{"If-None-Match": {etag}})
	assert.Equal(t, http.StatusNotModified, again.Code)
	assert.Empty(t, again.Body.String())
}

// The schema check has teeth: a card missing its identity, or naming a
// schema other than the v1 card schema, is rejected.
func TestServerCardSchemaRejectsNonConformingCards(t *testing.T) {
	schema, err := jsonschema.NewCompiler().Compile(filepath.Join("..", "..",
		"testdata", "schemas", "mcp", "server-card.schema.json") +
		"#/$defs/ServerCard")
	require.NoError(t, err)
	for _, raw := range []string{
		`{}`,
		`{"$schema":"https://example.com/card.json","name":"a/b",` +
			`"version":"1","description":"x"}`,
	} {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
		require.NoError(t, err)
		assert.Error(t, schema.Validate(doc), raw)
	}
}

func TestETagMatchesWeakAndListedTags(t *testing.T) {
	const etag = `"abc"`
	for header, want := range map[string]bool{
		`"abc"`:              true,
		`W/"abc"`:            true,
		`"x", "abc"`:         true,
		`*`:                  true,
		`"other"`:            false,
		``:                   false,
		`W/"x" , W/"abc"   `: true,
	} {
		assert.Equal(t, want, etagMatches(header, etag), header)
	}
}

// A long store name is bounded once, so the card and initialize agree.
func TestIdentityTitleIsBoundedForBothSurfaces(t *testing.T) {
	tn := cardTenant()
	tn.StoreName = strings.Repeat("Ω", 150)
	id := IdentityFor(tn)
	assert.Equal(t, 100, len([]rune(id.Title)))
}

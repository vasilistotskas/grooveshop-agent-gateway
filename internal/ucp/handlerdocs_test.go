package ucp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every document BuildProfile advertises must actually be served. A
// platform that cannot fetch a declared schema drops the entity, so a
// missing file silently disables payment rather than failing loudly.
func TestHandlerDocsServesEveryAdvertisedDocument(t *testing.T) {
	srv := httptest.NewServer(HandlerDocsHandler())
	t.Cleanup(srv.Close)

	h := paymentHandlers(tenantWith("cash_on_delivery"),
		"production")[HandlerName][0]

	for _, advertised := range []string{h.Spec, h.Schema} {
		path := strings.TrimPrefix(advertised, handlerBase)
		require.NotEqual(t, advertised, path, "not under handlerBase")

		resp, err := srv.Client().Get(srv.URL + "/" + HandlerVersion + path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		assert.Equal(t, http.StatusOK, resp.StatusCode, advertised)
	}
}

// The schemas the handler root references must resolve too — a platform
// composes the whole tree, not just the entry point.
func TestHandlerDocsSchemaTreeIsComplete(t *testing.T) {
	srv := httptest.NewServer(HandlerDocsHandler())
	t.Cleanup(srv.Close)

	get := func(path string) map[string]any {
		resp, err := srv.Client().Get(srv.URL + path)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode, path)
		assert.Contains(t, resp.Header.Get("Content-Type"), "json", path)

		var doc map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc), path)
		return doc
	}

	root := get("/" + HandlerVersion + "/schema.json")
	assert.Equal(t, HandlerName, root["name"])
	assert.Equal(t, HandlerVersion, root["version"])

	for _, rel := range []string{
		"types/cash_on_delivery_instrument.json",
		"types/business_config.json",
		"types/response_config.json",
	} {
		get("/" + HandlerVersion + "/" + rel)
	}
}

// Version-pinned documents never change at a URL, so they must be
// cacheable — a platform refetching them per checkout is wasted work.
func TestHandlerDocsAreCacheable(t *testing.T) {
	srv := httptest.NewServer(HandlerDocsHandler())
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/" + HandlerVersion + "/schema.json")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Contains(t, resp.Header.Get("Cache-Control"), "max-age=")
}

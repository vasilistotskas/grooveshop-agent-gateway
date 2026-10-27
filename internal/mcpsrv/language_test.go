package mcpsrv

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// A chat turn on /en asked Django for Greek: every tool call sent the
// store's default language, so product names came back Greek. A turn's
// language reaches each call it makes, as X-Language, the search's
// languageCode and the translation picked.
func TestToolsAskDjangoForTheTurnsLanguage(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx      func(context.Context) context.Context
		wantLang string
		wantName string
	}{
		"turn in English": {func(ctx context.Context) context.Context {
			return django.WithLanguage(ctx, "en")
		}, "en", "Scented Candles"},
		// /mcp names no language: the store's default.
		"no turn language": {func(ctx context.Context) context.Context {
			return ctx
		}, "el", "Portable Phone Charger"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := tc.ctx(
				tenant.NewContext(context.Background(), cartTenant()))

			var langs []string
			h := fixtureHandlers(t, "product_detail.json",
				func(r *http.Request) {
					langs = append(langs, r.Header.Get("X-Language"))
				})
			_, out, err := h.getProduct(ctx, nil,
				GetProductIn{ProductID: 1})
			require.NoError(t, err)
			assert.Equal(t, tc.wantName, out.Name)

			var codes []string
			search := fixtureHandlers(t, "search_product.json",
				func(r *http.Request) {
					langs = append(langs, r.Header.Get("X-Language"))
					codes = append(codes, r.URL.Query().Get("languageCode"))
				})
			_, _, err = search.searchProducts(ctx, nil,
				SearchProductsIn{Query: "phone"})
			require.NoError(t, err)

			require.NotEmpty(t, langs)
			for _, l := range langs {
				assert.Equal(t, tc.wantLang, l)
			}
			assert.Equal(t, []string{tc.wantLang}, codes)
		})
	}
}

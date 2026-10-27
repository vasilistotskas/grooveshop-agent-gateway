package mcpsrv

import (
	"context"
	"net/http"
	"strings"
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

// An English chat turn linked the Greek pages: the storefront serves its
// build default (el here) unprefixed and every other language under
// /<code>, so the links follow the turn's language, and /mcp, which
// names none, the store's default.
func TestLinksFollowTheTurnsLanguage(t *testing.T) {
	enStore := cartTenant()
	enStore.DefaultLocale = "en"
	for name, tc := range map[string]struct {
		ctx         context.Context
		wantProduct string
		wantClaim   string
		wantPolicy  string
	}{
		"English turn": {
			django.WithLanguage(
				tenant.NewContext(context.Background(), cartTenant()), "en"),
			"https://shop.example.test/en/products/",
			"https://shop.example.test/cart/claim?locale=en&uuid=" +
				fixtureCartID,
			"https://shop.example.test/en/return-policy",
		},
		"Greek turn": {
			django.WithLanguage(
				tenant.NewContext(context.Background(), cartTenant()), "el"),
			"https://shop.example.test/products/",
			"https://shop.example.test/cart/claim?uuid=" + fixtureCartID,
			"https://shop.example.test/return-policy",
		},
		"/mcp at an English-default store": {
			tenant.NewContext(context.Background(), enStore),
			"https://shop.example.test/en/products/",
			"https://shop.example.test/cart/claim?locale=en&uuid=" +
				fixtureCartID,
			"https://shop.example.test/en/return-policy",
		},
	} {
		t.Run(name, func(t *testing.T) {
			product := fixtureHandlers(t, "product_detail.json",
				func(*http.Request) {})
			_, p, err := product.getProduct(tc.ctx, nil,
				GetProductIn{ProductID: 1})
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(p.URL, tc.wantProduct), p.URL)

			cart, _ := cartHandlers(t, http.StatusOK)
			_, link, err := cart.getCheckoutLink(tc.ctx, nil,
				CheckoutLinkIn{CartID: fixtureCartID})
			require.NoError(t, err)
			assert.Equal(t, tc.wantClaim, link.URL)

			policies, _ := policiesDjango(t,
				`{"RETURN_WINDOW_DAYS": "14", "RETURN_FEES": "free"}`,
				http.StatusOK)
			_, out, err := policies.getStorePolicies(tc.ctx, nil,
				StorePoliciesIn{})
			require.NoError(t, err)
			assert.Equal(t, tc.wantPolicy, out.ReturnPolicyURL)
		})
	}
}

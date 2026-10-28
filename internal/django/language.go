package django

import (
	"context"
	"slices"
)

type languageKey struct{}

// WithLanguage scopes the calls made with ctx to lang: the language a
// chat turn is served in, which the storefront page the shopper is on
// chose. Tool handlers send it as X-Language and pick translations by
// it (Language), so product names come back in the page's language.
func WithLanguage(ctx context.Context, lang string) context.Context {
	return context.WithValue(ctx, languageKey{}, lang)
}

// Language is the language calls made with ctx are served in: the one
// WithLanguage set, else the store's default (an /mcp call, which names
// none).
func Language(ctx context.Context, t TenantConfig) string {
	if lang, ok := ctx.Value(languageKey{}).(string); ok && lang != "" {
		return lang
	}
	return t.DefaultLocale
}

// ServedLocales are the languages the store serves its storefront in:
// its availableLocales, or only its default when it lists none (Django's
// Tenant.served_locales).
func (t TenantConfig) ServedLocales() []string {
	if len(t.AvailableLocales) == 0 {
		return []string{t.DefaultLocale}
	}
	return t.AvailableLocales
}

// Serves reports whether the store serves its storefront in lang.
func (t TenantConfig) Serves(lang string) bool {
	return slices.Contains(t.ServedLocales(), lang)
}

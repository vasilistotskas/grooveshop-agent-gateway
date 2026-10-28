package django

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A store listing no languages serves its default alone, as Django's
// Tenant.served_locales does.
func TestServedLocalesWithoutAList(t *testing.T) {
	c := TenantConfig{DefaultLocale: "el"}

	assert.Equal(t, []string{"el"}, c.ServedLocales())
	assert.True(t, c.Serves("el"))
	assert.False(t, c.Serves("en"))
}

func TestLanguageIsTheTurnsElseTheStoresDefault(t *testing.T) {
	c := TenantConfig{DefaultLocale: "el"}

	assert.Equal(t, "el", Language(context.Background(), c))
	assert.Equal(t, "en",
		Language(WithLanguage(context.Background(), "en"), c))
}

package django

// DTOs are hand-written and hold only the fields the gateway consumes —
// unknown upstream fields are ignored by encoding/json. Drift against
// grooveshop-django-api/schema.yml is guarded by fixture-decode tests in
// dto_decode_test.go; refresh testdata/fixtures/django/ whenever a used
// endpoint's schema changes.

// TenantConfig mirrors the tenant/resolve wire shape (camelCase, produced
// by djangorestframework-camel-case from TenantConfigSerializer).
type TenantConfig struct {
	SchemaName           string `json:"schemaName"`
	Name                 string `json:"name"`
	StoreName            string `json:"storeName"`
	StoreDescription     string `json:"storeDescription"`
	LogoLightURL         string `json:"logoLightUrl"`
	LogoDarkURL          string `json:"logoDarkUrl"`
	FaviconURL           string `json:"faviconUrl"`
	DefaultLocale        string `json:"defaultLocale"`
	DefaultCurrency      string `json:"defaultCurrency"`
	PrimaryDomain        string `json:"primaryDomain"`
	LoyaltyEnabled       bool   `json:"loyaltyEnabled"`
	BlogEnabled          bool   `json:"blogEnabled"`
	StripePublishableKey string `json:"stripePublishableKey"`
}

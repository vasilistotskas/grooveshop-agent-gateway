package django

import (
	"bytes"
	"encoding/json"
)

// AIProfile is the store's per-tenant model provider configuration from
// Django's platform provider catalog. Django includes it only on
// internally-authenticated resolves (X-Internal-Token); nil means AI
// features are disabled for the store (no platform fallback).
// ReasoningEffort is the chat-completions reasoning_effort value the
// store chose, or "" to send none.
type AIProfile struct {
	Provider        string `json:"provider"`
	BaseURL         string `json:"baseUrl"`
	Model           string `json:"model"`
	APIKey          string `json:"apiKey"`
	ReasoningEffort string `json:"reasoningEffort"`
}

// TenantConfig mirrors the tenant/resolve wire shape (camelCase, produced
// by djangorestframework-camel-case from TenantConfigSerializer).
type TenantConfig struct {
	SchemaName    string `json:"schemaName"`
	Name          string `json:"name"`
	StoreName     string `json:"storeName"`
	FaviconURL    string `json:"faviconUrl"`
	DefaultLocale string `json:"defaultLocale"`
	// AvailableLocales are the languages the storefront serves the
	// store in; empty means its default alone (ServedLocales).
	AvailableLocales []string `json:"availableLocales" contract:"optional"`
	DefaultCurrency  string   `json:"defaultCurrency"`
	PrimaryDomain    string   `json:"primaryDomain"`
	// AssetsDomain is the tenant's OWN media host, and is empty for
	// every tenant that has not opted into white-label asset URLs —
	// which is the documented default. Resolve it through
	// media.Host so an empty value falls back to the platform origin
	// instead of producing assets.<tenant-domain>, a hostname standard
	// onboarding never creates.
	AssetsDomain string `json:"assetsDomain"`
	// Features is each optional store feature's EFFECTIVE state (the
	// plan flag AND the store's own settings), resolved by Django from
	// one table (tenant/features.py). A tool or surface that serves a
	// feature is offered only while it is on.
	Features StoreFeatures `json:"features"`
	// AgentCommerceEnabled / ProductFeedsEnabled gate this gateway's
	// surfaces per tenant. Django serves the EFFECTIVE value (plan
	// flag AND merchant extra-setting) on every resolve, and its cache
	// key includes the payload shape, so the fields are never absent;
	// a payload without them decodes as off. Use the
	// AgentCommerceOn/ProductFeedsOn accessors, which encode the
	// subordination, never the raw fields.
	AgentCommerceEnabled bool `json:"agentCommerceEnabled"`
	ProductFeedsEnabled  bool `json:"productFeedsEnabled"`
	// AIProfile is the tenant's own model provider, model, API key and
	// reasoning effort. Django includes it only on
	// internally-authenticated resolves (the X-Internal-Token header);
	// nil means AI features are off for the tenant.
	AIProfile *AIProfile `json:"aiProfile" contract:"undocumented"`
	// AgentPaymentInstruments lists the provider codes an agent can
	// settle unaided, in the merchant's presentation order. Django
	// derives it from the tenant's ACTIVE OFFLINE pay-ways, so every
	// entry needs no payment credential — the buyer settles with the
	// carrier. Empty means the store has nothing an agent can complete
	// on its own and checkout must escalate to the browser.
	AgentPaymentInstruments []string `json:"agentPaymentInstruments"`
	// AgentHostedPaymentEnabled is the EFFECTIVE two-tier gate (platform
	// plan flag AND merchant extra-setting) for letting an agent pick a
	// method the business settles on its own hosted page. Off degrades
	// gracefully — agents settle only what they can settle themselves
	// and card buyers are handed off, which is the plain UCP escalation
	// flow.
	AgentHostedPaymentEnabled bool `json:"agentHostedPaymentEnabled"`
	// ACPBearerToken authenticates the tenant's agentic-commerce platform
	// on /acp/*. Like AIProfile it arrives only on internally
	// authenticated resolves; empty means no platform is enrolled.
	ACPBearerToken string `json:"acpBearerToken" contract:"undocumented"`
}

// tenantResolve is the tenant/resolve body: the config, and whether Django
// recognised the caller's X-Internal-Token. Django adds acpBearerToken
// (possibly empty) only then (grooveshop-django-api tenant/views.py,
// tenant_resolve), so its key presence is the answer; aiProfile absent or
// null means the store has no AI profile (chat off). The flag is unexported
// and lives beside TenantConfig, never in it: the cached shape and the
// contract stay what they are.
type tenantResolve struct {
	TenantConfig
	recognised bool
}

func (r *tenantResolve) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&r.TenantConfig); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	_, r.recognised = keys["acpBearerToken"]
	return nil
}

// StoreFeatures is the part of TenantConfig.features the gateway reads:
// the features some tool or surface of it serves.
type StoreFeatures struct {
	// CustomerAccounts: shoppers can sign in, so an agent can link one's
	// account. Django folds it into every feature that needs an account
	// (reviews, favourites, loyalty), which read off with it.
	CustomerAccounts bool `json:"customerAccounts"`
	// Catalogue: the store publishes its products (listings, product
	// pages, search).
	Catalogue bool `json:"catalogue"`
	// Cart: the store takes orders. Django folds the catalogue into it.
	Cart           bool `json:"cart"`
	ProductReviews bool `json:"productReviews"`
	ProductAlerts  bool `json:"productAlerts"`
	Favourites     bool `json:"favourites"`
	Loyalty        bool `json:"loyalty"`
	Promotions     bool `json:"promotions"`
	// StaffAIAccess: the store lets its team connect their own AI app to
	// /mcp/admin. Off by default: what the app reads goes to its provider.
	StaffAIAccess bool `json:"staffAiAccess"`
}

// AgentCommerceOn reports whether the agent-commerce surface (MCP,
// UCP, ACP, chat) is enabled for the tenant.
func (t TenantConfig) AgentCommerceOn() bool {
	return t.AgentCommerceEnabled
}

// HostedPaymentOn reports whether an agent may choose a method the
// business settles on its own hosted page. Subordinate to the
// agent-commerce gate.
func (t TenantConfig) HostedPaymentOn() bool {
	return t.AgentCommerceEnabled && t.AgentHostedPaymentEnabled
}

// ProductFeedsOn reports whether catalog feed syndication is enabled.
// Subordinate to the agent-commerce gate, and to the catalogue: a feed
// of products the store does not publish lists pages that 404.
func (t TenantConfig) ProductFeedsOn() bool {
	return t.AgentCommerceEnabled && t.ProductFeedsEnabled &&
		t.Features.Catalogue
}

// CheckoutOn reports whether agents may check out at the store: the
// UCP profile and ACP sessions. Subordinate to the agent-commerce gate,
// and to the cart: a store that takes no orders has nothing to check
// out.
func (t TenantConfig) CheckoutOn() bool {
	return t.AgentCommerceEnabled && t.Features.Cart
}

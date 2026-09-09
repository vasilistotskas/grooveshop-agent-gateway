package django

import "encoding/json"

// DTOs are hand-written and hold only the fields the gateway consumes —
// unknown upstream fields are ignored by encoding/json. Drift against
// grooveshop-django-api/schema.yml is guarded by fixture-decode tests in
// dto_decode_test.go; refresh testdata/fixtures/django/ whenever a used
// endpoint's schema changes.

// Page is the DRF pagination envelope shared by every list endpoint.
type Page[T any] struct {
	Links struct {
		Next     *string `json:"next"`
		Previous *string `json:"previous"`
	} `json:"links"`
	Count      int64 `json:"count"`
	TotalPages int   `json:"totalPages"`
	PageSize   int   `json:"pageSize"`
	Page       int   `json:"page"`
	Results    []T   `json:"results"`
}

// Translation is the parler per-language payload for products/categories.
type Translation struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Product covers list and detail shapes; money and percentages stay
// json.Number end to end (COERCE_DECIMAL_TO_STRING=False upstream) and are
// formatted only at protocol edges. FinalPrice is VAT-inclusive.
type Product struct {
	ID           int64                  `json:"id"`
	Translations map[string]Translation `json:"translations"`
	Slug         string                 `json:"slug"`
	Category     int64                  `json:"category"`
	VariantGroup *int64                 `json:"variantGroup"`
	BrandName    *string                `json:"brandName"`
	Price        json.Number            `json:"price"`
	VatValue     json.Number            `json:"vatValue"`
	Stock        int                    `json:"stock"`
	Active       bool                   `json:"active"`
	Weight       *struct {
		Unit  string      `json:"unit"`
		Value json.Number `json:"value"`
	} `json:"weight"`
	DiscountPercent        json.Number `json:"discountPercent"`
	VatPercent             json.Number `json:"vatPercent"`
	FinalPrice             json.Number `json:"finalPrice"`
	MainImagePath          string      `json:"mainImagePath"`
	ReviewAverage          json.Number `json:"reviewAverage"`
	ReviewCount            int         `json:"reviewCount"`
	LikesCount             int         `json:"likesCount"`
	ViewCount              int         `json:"viewCount"`
	UUID                   string      `json:"uuid"`
	PriceDropAlertsEnabled bool        `json:"priceDropAlertsEnabled"`
}

// ProductVariants is the /product/{id}/variants payload. Axes stay raw: the
// gateway forwards them verbatim and the shape varies per variant group.
type ProductVariants struct {
	Axes     json.RawMessage `json:"axes"`
	Variants []Product       `json:"variants"`
}

// SearchHit is one /search/product result (ProductTranslationSerializer).
// The Meilisearch "formatted"/"matchesPosition" blobs are display sugar the
// gateway does not consume.
type SearchHit struct {
	ID              int64       `json:"id"`
	Slug            string      `json:"slug"`
	LanguageCode    string      `json:"languageCode"`
	Name            string      `json:"name"`
	Description     string      `json:"description"`
	Master          int64       `json:"master"`
	MainImagePath   string      `json:"mainImagePath"`
	ContentType     string      `json:"contentType"`
	FinalPrice      json.Number `json:"finalPrice"`
	Price           json.Number `json:"price"`
	DiscountPercent json.Number `json:"discountPercent"`
	Stock           int         `json:"stock"`
	LikesCount      int         `json:"likesCount"`
	ViewCount       int         `json:"viewCount"`
	ReviewAverage   json.Number `json:"reviewAverage"`
	VatPercent      json.Number `json:"vatPercent"`
}

type SearchProductResponse struct {
	Limit              int                         `json:"limit"`
	Offset             int                         `json:"offset"`
	EstimatedTotalHits int64                       `json:"estimatedTotalHits"`
	Results            []SearchHit                 `json:"results"`
	FacetDistribution  map[string]map[string]int64 `json:"facetDistribution"`
}

type TrendingResponse struct {
	WindowHours int    `json:"windowHours"`
	ContentType string `json:"contentType"`
	Results     []struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	} `json:"results"`
}

// Category is one node of the MPTT tree from /product/category/all.
type Category struct {
	ID           int64                  `json:"id"`
	Translations map[string]Translation `json:"translations"`
	Slug         string                 `json:"slug"`
	Active       bool                   `json:"active"`
	Parent       *int64                 `json:"parent"`
	Level        int                    `json:"level"`
	TreeID       int64                  `json:"treeId"`
}

// PayWayTranslation: name is an identifier-style label (e.g. VIVA_WALLET);
// description/instructions are customer-facing copy.
type PayWayTranslation struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
}

type PayWay struct {
	ID            int64                        `json:"id"`
	Translations  map[string]PayWayTranslation `json:"translations"`
	Active        bool                         `json:"active"`
	Cost          json.Number                  `json:"cost"`
	FreeThreshold json.Number                  `json:"freeThreshold"`
	SortOrder     int                          `json:"sortOrder"`
	ProviderCode  string                       `json:"providerCode"`
	// How the money changes hands. See the Settlement* constants in
	// enums.go for why this replaced the isOnlinePayment /
	// requiresConfirmation booleans outright. Branch on it through the
	// predicates below, never by comparing the raw string, so an absent
	// or unrecognised value stays fail-closed at every call site.
	Settlement string `json:"settlement"`
}

// IsOnlineSettlement reports whether the shopper pays at checkout.
//
// False for an absent or unrecognised settlement — a caller asking
// "is this online?" to gate a stricter path must not be told "no"
// because the field went missing, so pair it with HasKnownSettlement
// wherever the answer decides whether money can be taken.
func (p PayWay) IsOnlineSettlement() bool {
	return p.Settlement == SettlementOnline
}

// IsCollectedLater reports whether the money is collected after
// checkout — cash to a courier, a card at a carrier's locker terminal,
// or a bank transfer settled off-platform.
//
// This is the positive form on purpose. "Not online" would include the
// empty string, which is precisely the state a dropped upstream column
// produces; requiring a recognised value means such a pay way is simply
// never selected rather than silently treated as agent-completable.
func (p PayWay) IsCollectedLater() bool {
	switch p.Settlement {
	case SettlementCourierCash,
		SettlementCarrierTerminal,
		SettlementOfflineTransfer:
		return true
	default:
		return false
	}
}

// HasKnownSettlement reports whether the upstream payload carried a
// settlement this gateway understands. A false answer means the
// contract moved and the caller must refuse rather than guess.
func (p PayWay) HasKnownSettlement() bool {
	return p.IsOnlineSettlement() || p.IsCollectedLater()
}

type ShippingOption struct {
	ProviderCode string          `json:"providerCode"`
	ProviderName string          `json:"providerName"`
	Kind         string          `json:"kind"`
	Price        json.Number     `json:"price"`
	Currency     string          `json:"currency"`
	Priority     int             `json:"priority"`
	Metadata     json.RawMessage `json:"metadata"`
}

type FreeShippingInfo struct {
	Providers []struct {
		ProviderCode string      `json:"providerCode"`
		ProviderName string      `json:"providerName"`
		Kind         string      `json:"kind"`
		Threshold    json.Number `json:"threshold"`
		Priority     int         `json:"priority"`
	} `json:"providers"`
	MinThreshold json.Number `json:"minThreshold"`
	MaxThreshold json.Number `json:"maxThreshold"`
	Currency     string      `json:"currency"`
}

// AcsStation: coordinates and weights arrive as strings from ACS.
type AcsStation struct {
	ID           int64  `json:"id"`
	ExternalID   string `json:"externalId"`
	BranchCode   string `json:"branchCode"`
	ShopKind     int    `json:"shopKind"`
	Name         string `json:"name"`
	AddressLine1 string `json:"addressLine1"`
	City         string `json:"city"`
	PostalCode   string `json:"postalCode"`
	CountryCode  string `json:"countryCode"`
	Lat          string `json:"lat"`
	Lng          string `json:"lng"`
	MaxWeightKg  string `json:"maxWeightKg"`
	WorkingHours string `json:"workingHours"`
	IsActive     bool   `json:"isActive"`
}

// BoxNowLocker mirrors the boxnow lockers/nearest proxy response.
type BoxNowLocker struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Lat          string      `json:"lat"`
	Lng          string      `json:"lng"`
	Title        string      `json:"title"`
	Name         string      `json:"name"`
	PostalCode   string      `json:"postalCode"`
	Country      string      `json:"country"`
	Note         string      `json:"note"`
	AddressLine1 string      `json:"addressLine1"`
	AddressLine2 string      `json:"addressLine2"`
	Distance     json.Number `json:"distance"`
}

// Review deliberately decodes only the reviewer's first name: the upstream
// serializer embeds the full account object (email, phone, address) on an
// anonymous endpoint, and that data must never transit the gateway.
type Review struct {
	ID           int64 `json:"id"`
	Rate         int   `json:"rate"`
	Translations map[string]struct {
		Comment string `json:"comment"`
	} `json:"translations"`
	CreatedAt string `json:"createdAt"`
	User      struct {
		FirstName string `json:"firstName"`
	} `json:"user"`
}

// CartItem is one cart line; Product is embedded in full by the API.
type CartItem struct {
	ID              int64       `json:"id"`
	Product         Product     `json:"product"`
	Quantity        int         `json:"quantity"`
	FinalPrice      json.Number `json:"finalPrice"`
	TotalPrice      json.Number `json:"totalPrice"`
	DiscountPercent json.Number `json:"discountPercent"`
}

// Cart is identified by UUID; the API resolves it from the X-Cart-Id
// header and auto-creates one on first GET. TotalDiscountValue is the
// product-markdown savings already baked into item prices, while
// PromotionDiscount is the server-evaluated promotion/coupon discount
// still to subtract from TotalPrice.
type Cart struct {
	ID                    int64       `json:"id"`
	UUID                  string      `json:"uuid"`
	Items                 []CartItem  `json:"items"`
	TotalPrice            json.Number `json:"totalPrice"`
	TotalVatValue         json.Number `json:"totalVatValue"`
	TotalDiscountValue    json.Number `json:"totalDiscountValue"`
	PromotionDiscount     json.Number `json:"promotionDiscount"`
	PromotionFreeShipping bool        `json:"promotionFreeShipping"`
	AppliedCouponCodes    []string    `json:"appliedCouponCodes"`
	TotalItems            int         `json:"totalItems"`
	TotalItemsUnique      int         `json:"totalItemsUnique"`
	Currency              string      `json:"currency"`
}

type OrderItem struct {
	Product    Product     `json:"product"`
	Quantity   int         `json:"quantity"`
	Price      json.Number `json:"price"`
	TotalPrice json.Number `json:"totalPrice"`
}

type TrackingDetails struct {
	TrackingNumber    string  `json:"trackingNumber"`
	ShippingCarrier   string  `json:"shippingCarrier"`
	HasTracking       bool    `json:"hasTracking"`
	EstimatedDelivery *string `json:"estimatedDelivery"`
	TrackingURL       string  `json:"trackingUrl"`
}

type PricingBreakdown struct {
	ItemsSubtotal    json.Number `json:"itemsSubtotal"`
	ShippingCost     json.Number `json:"shippingCost"`
	PaymentMethodFee json.Number `json:"paymentMethodFee"`
	Discount         json.Number `json:"discount"`
	LoyaltyDiscount  json.Number `json:"loyaltyDiscount"`
	GiftCardAmount   json.Number `json:"giftCardAmount"`
	GrandTotal       json.Number `json:"grandTotal"`
	PaidAmount       json.Number `json:"paidAmount"`
	Currency         string      `json:"currency"`
}

// Order decodes the guest-accessible detail shape. Recipient contact and
// address fields are deliberately not decoded: possession of the UUID
// authorizes status tracking, not PII retrieval.
type Order struct {
	ID                   int64            `json:"id"`
	UUID                 string           `json:"uuid"`
	Status               string           `json:"status"`
	StatusDisplay        string           `json:"statusDisplay"`
	PaymentStatus        string           `json:"paymentStatus"`
	PaymentStatusDisplay string           `json:"paymentStatusDisplay"`
	IsPaid               bool             `json:"isPaid"`
	CanBeCanceled        bool             `json:"canBeCanceled"`
	Items                []OrderItem      `json:"items"`
	TrackingDetails      *TrackingDetails `json:"trackingDetails"`
	PricingBreakdown     PricingBreakdown `json:"pricingBreakdown"`
	CreatedAt            string           `json:"createdAt"`
}

type ProductAlert struct {
	ID          int64        `json:"id"`
	Kind        string       `json:"kind"`
	Product     int64        `json:"product"`
	Email       string       `json:"email"`
	TargetPrice *json.Number `json:"targetPrice"`
	IsActive    bool         `json:"isActive"`
}

// TenantConfig mirrors the tenant/resolve wire shape (camelCase, produced
// by djangorestframework-camel-case from TenantConfigSerializer).
type TenantConfig struct {
	SchemaName       string `json:"schemaName"`
	Name             string `json:"name"`
	StoreName        string `json:"storeName"`
	StoreDescription string `json:"storeDescription"`
	LogoLightURL     string `json:"logoLightUrl"`
	LogoDarkURL      string `json:"logoDarkUrl"`
	FaviconURL       string `json:"faviconUrl"`
	DefaultLocale    string `json:"defaultLocale"`
	DefaultCurrency  string `json:"defaultCurrency"`
	PrimaryDomain    string `json:"primaryDomain"`
	// AssetsDomain is the tenant's OWN media host, and is empty for
	// every tenant that has not opted into white-label asset URLs —
	// which is the documented default. Resolve it through
	// media.Host so an empty value falls back to the platform origin
	// instead of producing assets.<tenant-domain>, a hostname standard
	// onboarding never creates.
	AssetsDomain         string `json:"assetsDomain"`
	LoyaltyEnabled       bool   `json:"loyaltyEnabled"`
	BlogEnabled          bool   `json:"blogEnabled"`
	StripePublishableKey string `json:"stripePublishableKey"`
	// AgentCommerceEnabled / ProductFeedsEnabled gate this gateway's
	// surfaces per tenant. Django serves the EFFECTIVE value (plan
	// flag AND merchant extra-setting) on every resolve, and its cache
	// key includes the payload shape, so the fields are never absent;
	// a payload without them decodes as off. Use the
	// AgentCommerceOn/ProductFeedsOn accessors, which encode the
	// subordination, never the raw fields.
	AgentCommerceEnabled bool `json:"agentCommerceEnabled"`
	ProductFeedsEnabled  bool `json:"productFeedsEnabled"`
	// ChatAPIKey is the tenant's own model-provider credential. Django
	// includes it only on internally-authenticated resolves (the
	// X-Internal-Token header); empty means chat is off for the tenant.
	ChatAPIKey string `json:"chatApiKey"`
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
	// on /acp/*. Like ChatAPIKey it arrives only on internally
	// authenticated resolves; empty means no platform is enrolled.
	ACPBearerToken string `json:"acpBearerToken"`
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
// Subordinate to the agent-commerce gate.
func (t TenantConfig) ProductFeedsOn() bool {
	return t.AgentCommerceEnabled && t.ProductFeedsEnabled
}

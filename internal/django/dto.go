package django

import "encoding/json"

// DTOs are hand-written and hold only the fields the gateway consumes —
// unknown upstream fields are ignored by encoding/json, and a missing one
// silently zero-fills. contract/django-consumer.openapi.json is generated
// from these types (contract_test.go) and the deploy gate diffs it against
// each Django release's schema.yml, so a DTO change needs
// `go test ./internal/django -run TestDjangoConsumerContract -update`.
// `contract:"…"` tags record where Django's schema is looser than the Go
// type (see schemaGen). Fixture-decode tests in dto_decode_test.go cover
// decoding itself.

// Page is the DRF pagination envelope shared by every list endpoint.
type Page[T any] struct {
	Links struct {
		Next     *string `json:"next"`
		Previous *string `json:"previous"`
	} `json:"links" contract:"optional"`
	Count      int64 `json:"count"`
	TotalPages int   `json:"totalPages" contract:"optional"`
	PageSize   int   `json:"pageSize" contract:"optional"`
	Page       int   `json:"page" contract:"optional"`
	Results    []T   `json:"results"`
}

// Translation is the parler per-language payload for products/categories.
type Translation struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Product covers list and detail shapes; money and percentages stay
// json.Number end to end (COERCE_DECIMAL_TO_STRING=False upstream) and are
// formatted only at protocol edges.
type Product struct {
	ID           int64                  `json:"id"`
	Translations map[string]Translation `json:"translations"`
	Slug         string                 `json:"slug"`
	Category     int64                  `json:"category"`
	VariantGroup *int64                 `json:"variantGroup"`
	BrandName    *string                `json:"brandName"`
	Stock        int                    `json:"stock" contract:"optional"`
	Active       bool                   `json:"active" contract:"optional"`
	Weight       *struct {
		Unit  string      `json:"unit" contract:"optional"`
		Value json.Number `json:"value" contract:"optional"`
	} `json:"weight"`
	VatPercent json.Number `json:"vatPercent"`
	Pricing
	MainImagePath string `json:"mainImagePath"`
	// ImagePaths is every image in display order, the main one first,
	// in mainImagePath's (already percent-encoded) form.
	ImagePaths    []string           `json:"imagePaths"`
	Attributes    []ProductAttribute `json:"attributes"`
	ReviewAverage json.Number        `json:"reviewAverage"`
	ReviewCount   int                `json:"reviewCount"`
	LikesCount    int                `json:"likesCount"`
	ViewCount     int                `json:"viewCount"`
	UUID          string             `json:"uuid"`
	// VariantGroupTitle is the variant group's name in the request
	// language; null without a group or a name.
	VariantGroupTitle *string `json:"variantGroupTitle"`
}

// ProductAttribute is one of a product's attribute values, translated.
// IsVariant marks the axes its variant group differs by (Colour,
// Memory): the ones a shopper picks between.
type ProductAttribute struct {
	AttributeName string `json:"attributeName"`
	Value         string `json:"value"`
	IsVariant     bool   `json:"isVariant"`
}

// ProductDetail is the /product/{id} shape: the price-drop alert switch
// exists only on the single-product payload.
type ProductDetail struct {
	Product
	PriceDropAlertsEnabled bool `json:"priceDropAlertsEnabled"`
}

// Pricing is the consumer price of one unit as products and search hits
// carry it (grooveshop-django-api docs/product-pricing.md). EffectivePrice
// is VAT-inclusive and already net of unconditional automatic promotions:
// the unit price checkout charges. PriorPrice is the EU Omnibus reference
// (lowest price of the 30 days before the reduction began); it and
// AnnouncedDiscountPercent are set together, and only then may a
// reduction be shown. EffectivePrice is null until upstream records it.
type Pricing struct {
	EffectivePrice           json.Number `json:"effectivePrice" contract:"nullable"`
	PriorPrice               json.Number `json:"priorPrice" contract:"nullable"`
	AnnouncedDiscountPercent json.Number `json:"announcedDiscountPercent" contract:"nullable"`
}

// ProductVariants is the /product/{id}/variants payload. Axes stay raw: the
// gateway forwards them verbatim and the shape varies per variant group.
type ProductVariants struct {
	Axes     json.RawMessage  `json:"axes"`
	Variants []ProductVariant `json:"variants"`
}

// ProductVariant is one sibling in a variant group, trimmed upstream to a
// swatch card that carries the sibling's own price fields.
type ProductVariant struct {
	ID            int64                  `json:"id"`
	Translations  map[string]Translation `json:"translations"`
	Slug          string                 `json:"slug"`
	Active        bool                   `json:"active"`
	Stock         int                    `json:"stock"`
	MainImagePath string                 `json:"mainImagePath"`
	Pricing
}

// SearchHit is one /search/product result (ProductTranslationSerializer).
// The Meilisearch "formatted"/"matchesPosition" blobs are display sugar the
// gateway does not consume.
type SearchHit struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	LanguageCode  string `json:"languageCode"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Master        int64  `json:"master"`
	MainImagePath string `json:"mainImagePath"`
	ContentType   string `json:"contentType"`
	Pricing
	Stock         int         `json:"stock"`
	LikesCount    int         `json:"likesCount"`
	ViewCount     int         `json:"viewCount"`
	ReviewAverage json.Number `json:"reviewAverage" contract:"nullable"`
	VatPercent    json.Number `json:"vatPercent" contract:"nullable"`
}

type SearchProductResponse struct {
	Limit              int                         `json:"limit"`
	Offset             int                         `json:"offset"`
	EstimatedTotalHits int64                       `json:"estimatedTotalHits"`
	Results            []SearchHit                 `json:"results"`
	FacetDistribution  map[string]map[string]int64 `json:"facetDistribution" contract:"optional"`
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
	Active       bool                   `json:"active" contract:"optional"`
	Parent       *int64                 `json:"parent"`
	Level        int                    `json:"level"`
	TreeID       int64                  `json:"treeId"`
}

// PayWayTranslation is the customer-facing copy; the method's name is
// not translated upstream — it is derived from PayWay.Key (Label).
type PayWayTranslation struct {
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
}

type PayWay struct {
	ID int64 `json:"id"`
	// Key is the language-independent PayWayEnum value (VIVA_WALLET,
	// PAY_ON_DELIVERY, ...). Render it through Label, never raw.
	Key           string                       `json:"key"`
	Translations  map[string]PayWayTranslation `json:"translations"`
	Active        bool                         `json:"active" contract:"optional"`
	Cost          json.Number                  `json:"cost"`
	FreeThreshold json.Number                  `json:"freeThreshold"`
	SortOrder     int                          `json:"sortOrder" contract:"nullable"`
	ProviderCode  string                       `json:"providerCode" contract:"optional"`
	// How the money changes hands. See the Settlement* constants in
	// enums.go for why this replaced the isOnlinePayment /
	// requiresConfirmation booleans outright. Branch on it through the
	// predicates below, never by comparing the raw string, so an absent
	// or unrecognised value stays fail-closed at every call site.
	Settlement string `json:"settlement" contract:"optional"`
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

// ShippingOption is one (provider, kind) row priced by the store's
// shipping rate for CountryCode. An option over its rate's weight cap
// (ExceedsMaxWeight), or whose parcels the named cart does not fit
// (ExceedsParcelSize), is still listed so a storefront can explain why it
// is unavailable; Django refuses to create an order with it.
type ShippingOption struct {
	ProviderCode      string          `json:"providerCode"`
	ProviderName      string          `json:"providerName"`
	Kind              string          `json:"kind"`
	Price             json.Number     `json:"price"`
	Currency          string          `json:"currency"`
	Priority          int             `json:"priority"`
	CountryCode       string          `json:"countryCode"`
	MaxWeightGrams    *int            `json:"maxWeightGrams"`
	ExceedsMaxWeight  bool            `json:"exceedsMaxWeight"`
	ExceedsParcelSize bool            `json:"exceedsParcelSize"`
	Metadata          json.RawMessage `json:"metadata"`
}

// FitsCart reports whether Django would ship the cart this way: within
// the weight cap and in the carrier's parcels.
func (o ShippingOption) FitsCart() bool {
	return !o.ExceedsMaxWeight && !o.ExceedsParcelSize
}

type FreeShippingInfo struct {
	Providers []struct {
		ProviderCode string      `json:"providerCode"`
		ProviderName string      `json:"providerName"`
		Kind         string      `json:"kind"`
		Threshold    json.Number `json:"threshold"`
		Priority     int         `json:"priority"`
	} `json:"providers"`
	MinThreshold json.Number `json:"minThreshold" contract:"nullable"`
	MaxThreshold json.Number `json:"maxThreshold" contract:"nullable"`
	Currency     string      `json:"currency"`
	// CountryCode is the destination the thresholds were resolved for —
	// the requested one, or the store's first shippable country.
	CountryCode string `json:"countryCode" contract:"nullable"`
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
	Lat          string `json:"lat" contract:"nullable"`
	Lng          string `json:"lng" contract:"nullable"`
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
	ID         int64       `json:"id"`
	CartID     string      `json:"cartId"`
	Product    Product     `json:"product"`
	Quantity   int         `json:"quantity" contract:"optional"`
	FinalPrice json.Number `json:"finalPrice"`
	TotalPrice json.Number `json:"totalPrice"`
}

// Cart is identified by UUID; the API resolves it from the X-Cart-Id
// header and auto-creates one on first GET. TotalDiscountValue is the
// product-markdown savings already baked into item prices, while
// PromotionDiscount is the server-evaluated promotion/coupon discount
// still to subtract from TotalPrice.
type Cart struct {
	ID                    int64              `json:"id"`
	UUID                  string             `json:"uuid"`
	Items                 []CartItem         `json:"items"`
	TotalPrice            json.Number        `json:"totalPrice"`
	TotalVatValue         json.Number        `json:"totalVatValue"`
	TotalDiscountValue    json.Number        `json:"totalDiscountValue"`
	PromotionDiscount     json.Number        `json:"promotionDiscount"`
	PromotionFreeShipping bool               `json:"promotionFreeShipping"`
	AppliedPromotions     []AppliedPromotion `json:"appliedPromotions"`
	AppliedCoupons        []AppliedCoupon    `json:"appliedCoupons"`
	TotalItems            int                `json:"totalItems" contract:"untyped"`
	TotalItemsUnique      int                `json:"totalItemsUnique"`
	TotalWeightGrams      int                `json:"totalWeightGrams"`
	Currency              string             `json:"currency"`
}

// AppliedPromotion is one offer that took money off the cart, with its
// own amount; the amounts sum to Cart.PromotionDiscount. Code is the
// coupon that earned it, empty for an automatic promotion.
type AppliedPromotion struct {
	PromotionID int64       `json:"promotionId" contract:"optional"`
	Name        string      `json:"name" contract:"optional"`
	Code        string      `json:"code" contract:"optional,nullable"`
	Amount      json.Number `json:"amount" contract:"optional"`
}

// AppliedCoupon is Django's verdict on one attached coupon code: the money
// it takes off, the free shipping or gift it earns, or Rejection — the
// discount_code_* reason it is refused with now, empty while it applies.
// Django refuses a code that would change nothing, so exactly one holds.
type AppliedCoupon struct {
	Code         string      `json:"code"`
	Amount       json.Number `json:"amount"`
	FreeShipping bool        `json:"freeShipping"`
	Gift         bool        `json:"gift"`
	Rejection    string      `json:"rejection" contract:"nullable"`
}

type OrderItem struct {
	Product    Product     `json:"product"`
	Quantity   int         `json:"quantity" contract:"optional"`
	Price      json.Number `json:"price"`
	TotalPrice json.Number `json:"totalPrice"`
}

type TrackingDetails struct {
	TrackingNumber    string  `json:"trackingNumber" contract:"optional,nullable"`
	ShippingCarrier   string  `json:"shippingCarrier" contract:"optional,nullable"`
	HasTracking       bool    `json:"hasTracking" contract:"optional"`
	EstimatedDelivery *string `json:"estimatedDelivery"`
	TrackingURL       string  `json:"trackingUrl" contract:"optional,nullable"`
}

type PricingBreakdown struct {
	ItemsSubtotal    json.Number `json:"itemsSubtotal" contract:"optional"`
	ShippingCost     json.Number `json:"shippingCost" contract:"optional"`
	PaymentMethodFee json.Number `json:"paymentMethodFee" contract:"optional"`
	Discount         json.Number `json:"discount" contract:"optional"`
	LoyaltyDiscount  json.Number `json:"loyaltyDiscount" contract:"optional"`
	GiftCardAmount   json.Number `json:"giftCardAmount" contract:"optional"`
	GrandTotal       json.Number `json:"grandTotal" contract:"optional"`
	Currency         string      `json:"currency" contract:"optional"`
}

// Order decodes the guest-accessible detail shape. Recipient contact and
// address fields are deliberately not decoded: possession of the UUID
// authorizes status tracking, not PII retrieval.
type Order struct {
	ID                   int64            `json:"id"`
	UUID                 string           `json:"uuid"`
	Status               string           `json:"status" contract:"optional"`
	StatusDisplay        string           `json:"statusDisplay"`
	PaymentStatus        string           `json:"paymentStatus" contract:"optional,untyped"`
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
	Email       string       `json:"email" contract:"optional,nullable"`
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
	StripePublishableKey string `json:"stripePublishableKey"`
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
	// ChatAPIKey is the tenant's own model-provider credential. Django
	// includes it only on internally-authenticated resolves (the
	// X-Internal-Token header); empty means chat is off for the tenant.
	ChatAPIKey string `json:"chatApiKey" contract:"undocumented"`
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
	ACPBearerToken string `json:"acpBearerToken" contract:"undocumented"`
}

// StoreFeatures is the part of TenantConfig.features the gateway reads:
// the features some tool or surface of it serves.
type StoreFeatures struct {
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

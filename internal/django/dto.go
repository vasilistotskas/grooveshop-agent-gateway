package django

import (
	"encoding/json"
	"time"
)

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
	Count      int64 `json:"count"`
	TotalPages int   `json:"totalPages" contract:"optional"`
	Results    []T   `json:"results"`
}

// Translation is the parler per-language payload for products/categories.
type Translation struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// FeedTitle is the title the store wrote for product feeds, when it
	// differs from the name; a product's only.
	FeedTitle string `json:"feedTitle" contract:"optional"`
}

// Product covers list and detail shapes; money and percentages stay
// json.Number end to end (COERCE_DECIMAL_TO_STRING=False upstream) and are
// formatted only at protocol edges.
type Product struct {
	ID           int64                  `json:"id"`
	Translations map[string]Translation `json:"translations"`
	Slug         string                 `json:"slug"`
	// Category is nil for a product without one: Django's Product.category
	// is SET_NULL and optional in the admin.
	Category     *int64  `json:"category"`
	VariantGroup *int64  `json:"variantGroup"`
	BrandName    *string `json:"brandName"`
	// Gtin and Mpn are the identifiers the store entered; empty when it
	// has none.
	Gtin       string      `json:"gtin" contract:"optional"`
	Mpn        string      `json:"mpn" contract:"optional"`
	Stock      int         `json:"stock" contract:"optional"`
	Active     bool        `json:"active" contract:"optional"`
	VatPercent json.Number `json:"vatPercent"`
	Pricing
	MainImagePath string `json:"mainImagePath"`
	// ImagePaths is every image in display order, the main one first,
	// in mainImagePath's (already percent-encoded) form.
	ImagePaths    []string           `json:"imagePaths"`
	Attributes    []ProductAttribute `json:"attributes"`
	ReviewAverage json.Number        `json:"reviewAverage"`
	ReviewCount   int                `json:"reviewCount"`
	UUID          string             `json:"uuid"`
	// VariantGroupTitle is the variant group's name in the request
	// language; null without a group or a name.
	VariantGroupTitle *string `json:"variantGroupTitle"`
}

// ProductFeed is a row of /product/feed: a product and what only the feeds
// read. Highlights are its selling points in the request language, in the
// store's order.
type ProductFeed struct {
	Product
	Highlights []string `json:"highlights"`
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

// ProductVariants is the /product/{id}/variants payload.
type ProductVariants struct {
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
	ReviewAverage json.Number `json:"reviewAverage" contract:"nullable"`
	VatPercent    json.Number `json:"vatPercent" contract:"nullable"`
}

type SearchProductResponse struct {
	Limit              int         `json:"limit"`
	Offset             int         `json:"offset"`
	EstimatedTotalHits int64       `json:"estimatedTotalHits"`
	Results            []SearchHit `json:"results"`
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
}

// RegionTranslation is a region's name in one language. A store may hold
// a region in only some of its languages.
type RegionTranslation struct {
	Name string `json:"name" contract:"optional"`
}

// Region is a delivery region of a country. Alpha is what an order names
// it by (OrderCreate.RegionID).
type Region struct {
	Alpha        string                       `json:"alpha"`
	Translations map[string]RegionTranslation `json:"translations"`
}

// PayWayTranslation is the customer-facing copy; the method's name is
// not translated upstream — it is derived from PayWay.Key (Label).
type PayWayTranslation struct {
	Description string `json:"description"`
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
	ProviderCode      string      `json:"providerCode"`
	ProviderName      string      `json:"providerName"`
	Kind              string      `json:"kind"`
	Price             json.Number `json:"price"`
	Currency          string      `json:"currency"`
	CountryCode       string      `json:"countryCode"`
	ExceedsMaxWeight  bool        `json:"exceedsMaxWeight"`
	ExceedsParcelSize bool        `json:"exceedsParcelSize"`
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
	} `json:"providers"`
	Currency string `json:"currency"`
	// CountryCode is the destination the thresholds were resolved for —
	// the requested one, or the store's first shippable country.
	CountryCode string `json:"countryCode" contract:"nullable"`
}

// AcsStation: coordinates and weights arrive as strings from ACS.
type AcsStation struct {
	ID           int64  `json:"id"`
	ExternalID   string `json:"externalId"`
	BranchCode   string `json:"branchCode"`
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
	ID           string `json:"id"`
	Type         string `json:"type"`
	Lat          string `json:"lat"`
	Lng          string `json:"lng"`
	Title        string `json:"title"`
	Name         string `json:"name"`
	PostalCode   string `json:"postalCode"`
	Country      string `json:"country"`
	Note         string `json:"note"`
	AddressLine1 string `json:"addressLine1"`
	AddressLine2 string `json:"addressLine2"`
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
// still to subtract from TotalPrice. FreeThresholdValue is what Django
// judges every "free above X" on, free shipping and a pay way's fee
// alike: the items after PromotionDiscount, never TotalPrice.
type Cart struct {
	ID                    int64              `json:"id"`
	UUID                  string             `json:"uuid"`
	Items                 []CartItem         `json:"items"`
	TotalPrice            json.Number        `json:"totalPrice"`
	TotalDiscountValue    json.Number        `json:"totalDiscountValue"`
	PromotionDiscount     json.Number        `json:"promotionDiscount"`
	FreeThresholdValue    json.Number        `json:"freeThresholdValue"`
	PromotionFreeShipping bool               `json:"promotionFreeShipping"`
	AppliedPromotions     []AppliedPromotion `json:"appliedPromotions"`
	AppliedCoupons        []AppliedCoupon    `json:"appliedCoupons"`
	TotalItems            int                `json:"totalItems" contract:"untyped"`
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
	Rejection    string      `json:"rejection" contract:"nullable"`
}

// OrderItem is one line of a placed order. ProductName and SKU are what the
// line was sold as, in the order's language, fixed when it was created: a
// renderer shows those, never the live Product's name, which can change
// after the sale. Product stays for the live id, links and image.
type OrderItem struct {
	Product     Product     `json:"product"`
	ProductName string      `json:"productName"`
	SKU         string      `json:"sku"`
	Quantity    int         `json:"quantity" contract:"optional"`
	Price       json.Number `json:"price"`
	TotalPrice  json.Number `json:"totalPrice"`
}

type TrackingDetails struct {
	TrackingNumber  string `json:"trackingNumber" contract:"optional,nullable"`
	ShippingCarrier string `json:"shippingCarrier" contract:"optional,nullable"`
	HasTracking     bool   `json:"hasTracking" contract:"optional"`
	TrackingURL     string `json:"trackingUrl" contract:"optional,nullable"`
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
	ID                   int64  `json:"id"`
	UUID                 string `json:"uuid"`
	Status               string `json:"status" contract:"optional"`
	StatusDisplay        string `json:"statusDisplay"`
	PaymentStatus        string `json:"paymentStatus" contract:"optional,untyped"`
	PaymentStatusDisplay string `json:"paymentStatusDisplay"`
	IsPaid               bool   `json:"isPaid"`
	// PaymentHoldExpiresAt is when the order's payment link stops being
	// payable, set only while it waits for an online payment.
	// PaymentDecidedBy is when Django's unpaid-order sweep will have
	// decided it: canceled it if it is still unpaid. Empty when nothing
	// will (no online payment awaited, a payment being processed, a
	// charge that did not match, the sweep off). RFC 3339, null as "".
	PaymentHoldExpiresAt string           `json:"paymentHoldExpiresAt" contract:"nullable"`
	PaymentDecidedBy     string           `json:"paymentDecidedBy" contract:"nullable"`
	Items                []OrderItem      `json:"items"`
	TrackingDetails      *TrackingDetails `json:"trackingDetails"`
	PricingBreakdown     PricingBreakdown `json:"pricingBreakdown"`
	CreatedAt            string           `json:"createdAt"`
}

// HoldExpiresAt is PaymentHoldExpiresAt as a time, zero for none.
func (o *Order) HoldExpiresAt() (time.Time, error) {
	return instant(o.PaymentHoldExpiresAt)
}

// DecidedBy is PaymentDecidedBy as a time, zero for none.
func (o *Order) DecidedBy() (time.Time, error) {
	return instant(o.PaymentDecidedBy)
}

// instant reads a nullable date-time Django sends as RFC 3339.
func instant(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, v)
}

type ProductAlert struct {
	ID          int64        `json:"id"`
	Kind        string       `json:"kind"`
	Product     int64        `json:"product"`
	Email       string       `json:"email" contract:"optional,nullable"`
	TargetPrice *json.Number `json:"targetPrice"`
	IsActive    bool         `json:"isActive"`
}

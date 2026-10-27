package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Price is the unit price every product-shaped tool result carries.
// Prices are decimal strings, VAT-inclusive. A reduction is stated only as
// upstream announces it (EU Omnibus: against the lowest price of the 30
// days before it began), never derived here.
type Price struct {
	EffectivePrice           string `json:"effectivePrice,omitempty" jsonschema:"VAT-inclusive unit price the shopper pays now, automatic promotions included; absent while the store is re-pricing the product"`
	PriorPrice               string `json:"priorPrice,omitempty" jsonschema:"present only for an announced reduction: the lowest price of the 30 days before it began, the only previous price that may be quoted"`
	AnnouncedDiscountPercent string `json:"announcedDiscountPercent,omitempty" jsonschema:"percent off priorPrice, present only with it"`
	Currency                 string `json:"currency"`
}

// ProductSummary is the compact product shape shared by search results and
// variant listings.
type ProductSummary struct {
	ID          int64  `json:"id" jsonschema:"product id for get_product"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Price
	Stock    int    `json:"stock"`
	InStock  bool   `json:"inStock"`
	Rating   string `json:"rating,omitempty"`
	URL      string `json:"url"`
	ImageURL string `json:"imageUrl,omitempty"`
}

// priceOf copies upstream pricing verbatim: an absent prior price means no
// reduction may be announced, whatever the merchant's markdown.
func priceOf(p django.Pricing, currency string) Price {
	out := Price{
		EffectivePrice: p.EffectivePrice.String(),
		Currency:       currency,
	}
	if p.PriorPrice != "" {
		out.PriorPrice = p.PriorPrice.String()
		out.AnnouncedDiscountPercent = p.AnnouncedDiscountPercent.String()
	}
	return out
}

// searchSorts are the search sort values this tool offers, all accepted
// by Django; the jsonschema tag on SearchProductsIn.Sort lists the same.
var searchSorts = []string{
	"effectivePrice", "-effectivePrice", "likesCount", "-likesCount",
	"viewCount", "-viewCount", "createdAt", "-createdAt",
}

type SearchProductsIn struct {
	Query      string  `json:"query" jsonschema:"free-text search, Greek or English"`
	Categories []int64 `json:"categories,omitempty" jsonschema:"category ids from list_categories"`
	PriceMin   string  `json:"priceMin,omitempty" jsonschema:"minimum VAT-inclusive price, decimal string"`
	PriceMax   string  `json:"priceMax,omitempty" jsonschema:"maximum VAT-inclusive price, decimal string"`
	Sort       string  `json:"sort,omitempty" jsonschema:"one of effectivePrice,-effectivePrice,likesCount,-likesCount,viewCount,-viewCount,createdAt,-createdAt"`
	Limit      int     `json:"limit,omitempty" jsonschema:"max results, default 10, cap 20"`
	Offset     int     `json:"offset,omitempty"`
}

type SearchProductsOut struct {
	TotalHits int64            `json:"totalHits"`
	Products  []ProductSummary `json:"products"`
}

func (h *handlers) searchProducts(
	ctx context.Context, _ *mcp.CallToolRequest, in SearchProductsIn,
) (*mcp.CallToolResult, SearchProductsOut, error) {
	var out SearchProductsOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)
	if in.Query == "" {
		return nil, out, errors.New("query is required")
	}
	if in.Sort != "" && !slices.Contains(searchSorts, in.Sort) {
		return nil, out, fmt.Errorf("sort must be one of %s",
			strings.Join(searchSorts, ", "))
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 20 {
		limit = 20
	}

	res, err := h.deps.Django.SearchProducts(ctx, t.Domain, django.SearchParams{
		Query:        in.Query,
		LanguageCode: lang,
		Categories:   in.Categories,
		PriceMin:     in.PriceMin,
		PriceMax:     in.PriceMax,
		Sort:         in.Sort,
		Limit:        limit,
		Offset:       in.Offset,
	})
	if err != nil {
		return nil, out, upstreamErr(err, "search is unavailable right now")
	}

	out.TotalHits = res.EstimatedTotalHits
	out.Products = make([]ProductSummary, 0, len(res.Results))
	for _, hit := range res.Results {
		s := ProductSummary{
			// Search hits are translation rows; Master is the product id.
			ID:          hit.Master,
			Name:        hit.Name,
			Description: truncate(hit.Description, 200),
			Price:       priceOf(hit.Pricing, t.DefaultCurrency),
			Stock:       hit.Stock,
			InStock:     hit.Stock > 0,
			Rating:      ratingOf(t, hit.ReviewAverage),
			URL:         storefront.Product(t.Domain, hit.Master, hit.Slug),
			ImageURL:    h.imageURL(t, hit.MainImagePath),
		}
		out.Products = append(out.Products, s)
	}

	return textResult(
		"Found %d of %d matching products for %q. Prices are "+
			"VAT-inclusive %s. Quote a previous price or discount only "+
			"where priorPrice is given. Use get_product with a product "+
			"id for full details.",
		len(out.Products), out.TotalHits, in.Query, t.DefaultCurrency,
	), out, nil
}

type GetProductIn struct {
	ProductID int64 `json:"productId" jsonschema:"product id from search_products or a product URL"`
}

type GetProductOut struct {
	ProductSummary
	VatPercent          string           `json:"vatPercent"`
	ReviewCount         *int             `json:"reviewCount,omitempty" jsonschema:"absent when the store has reviews off"`
	Brand               string           `json:"brand,omitempty"`
	PriceAlertAvailable bool             `json:"priceAlertAvailable"`
	Variants            []ProductSummary `json:"variants,omitempty"`
}

func (h *handlers) getProduct(
	ctx context.Context, _ *mcp.CallToolRequest, in GetProductIn,
) (*mcp.CallToolResult, GetProductOut, error) {
	var out GetProductOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)
	if in.ProductID <= 0 {
		return nil, out, errors.New("productId is required")
	}

	p, err := h.deps.Django.GetProduct(ctx, t.Domain, lang, in.ProductID)
	if err != nil {
		return nil, out, upstreamErr(err, fmt.Sprintf(
			"product %d was not found; find products with search_products",
			in.ProductID))
	}

	tr := django.Localized(p.Translations, lang)
	out.ProductSummary = ProductSummary{
		ID:          p.ID,
		Name:        tr.Name,
		Description: truncate(tr.Description, 600),
		Price:       priceOf(p.Pricing, t.DefaultCurrency),
		Stock:       p.Stock,
		InStock:     p.Stock > 0 && p.Active,
		Rating:      ratingOf(t, p.ReviewAverage),
		URL:         storefront.Product(t.Domain, p.ID, p.Slug),
		ImageURL:    h.imageURL(t, p.MainImagePath),
	}
	out.VatPercent = num(p.VatPercent)
	if t.Features.ProductReviews {
		out.ReviewCount = &p.ReviewCount
	}
	if p.BrandName != nil {
		out.Brand = *p.BrandName
	}
	out.PriceAlertAvailable = p.PriceDropAlertsEnabled

	variants, err := h.deps.Django.GetProductVariants(
		ctx, t.Domain, lang, in.ProductID)
	if err == nil {
		for _, v := range variants.Variants {
			if v.ID == p.ID {
				continue
			}
			vtr := django.Localized(v.Translations, lang)
			vs := ProductSummary{
				ID:       v.ID,
				Name:     vtr.Name,
				Price:    priceOf(v.Pricing, t.DefaultCurrency),
				Stock:    v.Stock,
				InStock:  v.Stock > 0 && v.Active,
				URL:      storefront.Product(t.Domain, v.ID, v.Slug),
				ImageURL: h.imageURL(t, v.MainImagePath),
			}
			out.Variants = append(out.Variants, vs)
		}
	}

	return textResult(
		"%s — %s, %s (%d units).%s %s",
		out.Name, priceNote(&out.Price), stockNote(out.InStock), out.Stock,
		ratingNote(t, out.Rating, p.ReviewCount), out.URL,
	), out, nil
}

// ratingOf is a product's average review rating, on Django's 1–10 scale,
// or empty for a store that switched reviews off: its reviews are not
// served, so neither is a number made of them.
func ratingOf(t *tenant.Tenant, average json.Number) string {
	if !t.Features.ProductReviews {
		return ""
	}
	return num(average)
}

// ratingNote is the rating sentence of a product's text, empty for a
// store with reviews off.
func ratingNote(t *tenant.Tenant, rating string, count int) string {
	if !t.Features.ProductReviews {
		return ""
	}
	return fmt.Sprintf(" Rated %s/10 from %d reviews.", rating, count)
}

func stockNote(inStock bool) string {
	if inStock {
		return "in stock"
	}
	return "OUT OF STOCK"
}

// priceNote words the price for the tool's text result; the reduction
// sentence exists only when upstream announced one.
func priceNote(s *Price) string {
	if s.EffectivePrice == "" {
		return "price being updated, see the product page"
	}
	note := fmt.Sprintf("%s %s (VAT included)", s.EffectivePrice, s.Currency)
	if s.PriorPrice != "" {
		note += fmt.Sprintf(", %s%% off %s %s, its lowest price in the "+
			"30 days before the reduction", s.AnnouncedDiscountPercent,
			s.PriorPrice, s.Currency)
	}
	return note
}

// CategoryOut is deliberately flat: recursive types cannot carry an MCP
// output schema, and agents navigate parentId/level just as well.
type CategoryOut struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ParentID *int64 `json:"parentId,omitempty" jsonschema:"absent on top-level categories"`
	Level    int    `json:"level"`
}

type ListCategoriesOut struct {
	Categories []CategoryOut `json:"categories"`
}

func (h *handlers) listCategories(
	ctx context.Context, _ *mcp.CallToolRequest, _ struct{},
) (*mcp.CallToolResult, ListCategoriesOut, error) {
	var out ListCategoriesOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)

	cats, err := h.deps.Django.ListAllCategories(ctx, t.Domain, lang)
	if err != nil {
		return nil, out, upstreamErr(err, "categories are unavailable")
	}

	topLevel := 0
	for _, c := range cats {
		if !c.Active {
			continue
		}
		if c.Parent == nil {
			topLevel++
		}
		out.Categories = append(out.Categories, CategoryOut{
			ID:       c.ID,
			Name:     django.Localized(c.Translations, lang).Name,
			ParentID: c.Parent,
			Level:    c.Level,
		})
	}
	return textResult(
		"The store has %d categories (%d top-level; children reference "+
			"their parentId). Pass category ids to search_products to "+
			"narrow results.",
		len(out.Categories), topLevel,
	), out, nil
}

type TrendingIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"max queries, default 8, cap 20"`
}

type TrendingOut struct {
	WindowHours int `json:"windowHours"`
	Queries     []struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	} `json:"queries"`
}

func (h *handlers) getTrendingSearches(
	ctx context.Context, _ *mcp.CallToolRequest, in TrendingIn,
) (*mcp.CallToolResult, TrendingOut, error) {
	var out TrendingOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)

	res, err := h.deps.Django.Trending(ctx, t.Domain, lang, in.Limit)
	if err != nil {
		return nil, out, upstreamErr(err, "trending searches are unavailable")
	}
	out.WindowHours = res.WindowHours
	for _, q := range res.Results {
		out.Queries = append(out.Queries, struct {
			Query string `json:"query"`
			Count int    `json:"count"`
		}{q.Query, q.Count})
	}
	return textResult(
		"%d trending search queries over the last %d hours.",
		len(out.Queries), out.WindowHours,
	), out, nil
}

type ReviewsIn struct {
	ProductID int64 `json:"productId"`
	Limit     int   `json:"limit,omitempty" jsonschema:"max reviews, default 5, cap 20"`
}

type ReviewsOut struct {
	Average string `json:"average"`
	Count   int64  `json:"count"`
	Reviews []struct {
		Reviewer string `json:"reviewer"`
		Rating   int    `json:"rating"`
		Comment  string `json:"comment"`
		Date     string `json:"date"`
	} `json:"reviews"`
}

func (h *handlers) getProductReviews(
	ctx context.Context, _ *mcp.CallToolRequest, in ReviewsIn,
) (*mcp.CallToolResult, ReviewsOut, error) {
	var out ReviewsOut
	t, err := h.tenantFor(ctx)
	if err != nil {
		return nil, out, err
	}
	lang := django.Language(ctx, t.TenantConfig)
	if in.ProductID <= 0 {
		return nil, out, errors.New("productId is required")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	p, err := h.deps.Django.GetProduct(ctx, t.Domain, lang, in.ProductID)
	if err != nil {
		return nil, out, upstreamErr(err, fmt.Sprintf(
			"product %d was not found", in.ProductID))
	}
	page, err := h.deps.Django.GetProductReviews(
		ctx, t.Domain, lang, in.ProductID, limit)
	if err != nil {
		return nil, out, upstreamErr(err, "reviews are unavailable")
	}

	out.Average = num(p.ReviewAverage)
	out.Count = page.Count
	for _, r := range page.Results {
		// Only rating, comment and first name leave the gateway — the
		// upstream payload carries reviewer PII that must not propagate.
		out.Reviews = append(out.Reviews, struct {
			Reviewer string `json:"reviewer"`
			Rating   int    `json:"rating"`
			Comment  string `json:"comment"`
			Date     string `json:"date"`
		}{
			Reviewer: r.User.FirstName,
			Rating:   r.Rate,
			Comment:  django.Localized(r.Translations, lang).Comment,
			Date:     r.CreatedAt,
		})
	}
	return textResult(
		"Rated %s/10 across %d reviews; showing %d.",
		out.Average, out.Count, len(out.Reviews),
	), out, nil
}

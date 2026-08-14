package django

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// SearchParams maps to /search/product query parameters. Query keys are
// camelCase on the wire (Django's CamelCaseMiddleWare underscoreizes them).
type SearchParams struct {
	Query        string
	LanguageCode string
	Categories   []int64
	PriceMin     string
	PriceMax     string
	Sort         string
	Facets       []string
	Limit        int
	Offset       int
}

func (p SearchParams) values() url.Values {
	q := url.Values{"query": {p.Query}}
	if p.LanguageCode != "" {
		q.Set("languageCode", p.LanguageCode)
	}
	if len(p.Categories) > 0 {
		ids := make([]string, len(p.Categories))
		for i, id := range p.Categories {
			ids[i] = strconv.FormatInt(id, 10)
		}
		q.Set("categories", strings.Join(ids, ","))
	}
	if p.PriceMin != "" {
		q.Set("priceMin", p.PriceMin)
	}
	if p.PriceMax != "" {
		q.Set("priceMax", p.PriceMax)
	}
	if p.Sort != "" {
		q.Set("sort", p.Sort)
	}
	if len(p.Facets) > 0 {
		q.Set("facets", strings.Join(p.Facets, ","))
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Offset > 0 {
		q.Set("offset", strconv.Itoa(p.Offset))
	}
	return q
}

func (c *Client) SearchProducts(
	ctx context.Context, host string, p SearchParams,
) (*SearchProductResponse, error) {
	var out SearchProductResponse
	err := c.get(ctx, request{
		path:     "/search/product",
		host:     host,
		language: p.LanguageCode,
		query:    p.values(),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Trending(
	ctx context.Context, host, lang string, limit int,
) (*TrendingResponse, error) {
	q := url.Values{"contentType": {"product"}}
	if lang != "" {
		q.Set("languageCode", lang)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out TrendingResponse
	err := c.get(ctx, request{
		path:     "/search/trending",
		host:     host,
		language: lang,
		query:    q,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetProduct(
	ctx context.Context, host, lang string, id int64,
) (*Product, error) {
	var out Product
	err := c.get(ctx, request{
		path:     "/product/{id}",
		rawPath:  fmt.Sprintf("/product/%d", id),
		host:     host,
		language: lang,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetProductVariants(
	ctx context.Context, host, lang string, id int64,
) (*ProductVariants, error) {
	var out ProductVariants
	err := c.get(ctx, request{
		path:     "/product/{id}/variants",
		rawPath:  fmt.Sprintf("/product/%d/variants", id),
		host:     host,
		language: lang,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetProductReviews(
	ctx context.Context, host, lang string, id int64, pageSize int,
) (*Page[Review], error) {
	q := url.Values{}
	if pageSize > 0 {
		q.Set("pageSize", strconv.Itoa(pageSize))
	}
	var out Page[Review]
	err := c.get(ctx, request{
		path:     "/product/{id}/reviews",
		rawPath:  fmt.Sprintf("/product/%d/reviews", id),
		host:     host,
		language: lang,
		query:    q,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAllCategories returns the full unpaginated MPTT tree, ordered by
// (treeId, lft) upstream — parents always precede their children.
func (c *Client) ListAllCategories(
	ctx context.Context, host, lang string,
) ([]Category, error) {
	var out []Category
	err := c.get(ctx, request{
		path:     "/product/category/all",
		host:     host,
		language: lang,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PayWays lists active payment methods, optionally filtered by carrier
// compatibility (providerCode + kind), mirroring the storefront checkout.
func (c *Client) PayWays(
	ctx context.Context, host, lang, shippingProviderCode, shippingKind string,
) (*Page[PayWay], error) {
	q := url.Values{}
	if shippingProviderCode != "" {
		q.Set("shippingProviderCode", shippingProviderCode)
	}
	if shippingKind != "" {
		q.Set("shippingKind", shippingKind)
	}
	var out Page[PayWay]
	err := c.get(ctx, request{
		path:     "/pay_way",
		host:     host,
		language: lang,
		query:    q,
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

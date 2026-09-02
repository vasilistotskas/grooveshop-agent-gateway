package feeds

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/media"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
)

// The RSS dialect (RSS 2.0 + xmlns:g) is byte-compatible with the feed the
// storefront used to render: Meta Commerce Manager, TikTok Ads Manager and
// Google Merchant Center all ingest it. Spec constraints preserved:
//   - price format "12.34 EUR" — dot decimal, space, ISO 4217 code
//   - availability enum: "in stock" | "out of stock"
//   - images JPEG/PNG only (never WebP), >=500x500 — the feed image
//     template pins 1000x1000 JPEG on white
//   - g:id equals the pixel/CAPI content_ids — always the product id,
//     never sku/uuid
const (
	feedTitleMax       = 200
	feedDescriptionMax = 9999
)

// feedContext carries per-generation tenant data every writer needs.
type feedContext struct {
	StoreName string
	// Domain is the storefront host, used for product links.
	Domain string
	// AssetsHost is the resolved media origin for image links — the
	// tenant's own when it opted into white-label assets, otherwise the
	// platform origin. NOT derived from Domain: assets.<storefront> is a
	// hostname standard onboarding never creates.
	AssetsHost       string
	Schema           string
	Currency         string
	Locale           string
	ImageURLTemplate string
	CategoryNames    map[int64]string
}

// feedItem is the normalized per-product row all writers consume.
type feedItem struct {
	ID          int64
	Title       string
	Description string
	Link        string
	ImageLink   string
	InStock     bool
	// Prices are ISO 4217 minor units, like everywhere else in the
	// gateway; formatFeedPrice renders them at the edge.
	RegularMinor int64
	SaleMinor    int64
	HasSalePrice bool
	Brand        string
	CategoryName string
	VariantGroup *int64
	Currency     string
}

// newFeedItem maps a product row; a nil item means the platforms would
// reject the product anyway (no default-locale name or no image),
// matching the previous implementation's skip rule. A malformed money
// field is an error: it would corrupt every consumer's feed.
func newFeedItem(p *django.Product, ctx *feedContext) (*feedItem, error) {
	tr := p.Translations[ctx.Locale]
	if tr.Name == "" || p.MainImagePath == "" {
		return nil, nil
	}
	title := text.Runes(tr.Name, feedTitleMax)
	desc := strings.TrimSpace(decodeHTMLEntities(stripHTMLTags(tr.Description)))
	if desc == "" {
		desc = tr.Name
	}
	desc = text.Runes(desc, feedDescriptionMax)

	price, err := money.MinorUnits(p.Price.String())
	if err != nil {
		return nil, err
	}
	vat, err := money.MinorUnits(p.VatValue.String())
	if err != nil {
		return nil, err
	}
	final, err := money.MinorUnits(p.FinalPrice.String())
	if err != nil {
		return nil, err
	}
	discount, _ := p.DiscountPercent.Float64()
	regular := price + vat

	brand := ctx.StoreName
	if p.BrandName != nil && *p.BrandName != "" {
		brand = *p.BrandName
	}

	return &feedItem{
		ID:    p.ID,
		Title: title, Description: desc,
		Link:         storefront.Product(ctx.Domain, p.ID, p.Slug),
		ImageLink:    feedImageURL(ctx, p.MainImagePath),
		InStock:      p.Stock > 0,
		RegularMinor: regular,
		SaleMinor:    final,
		HasSalePrice: discount > 0 && final < regular,
		Brand:        brand,
		CategoryName: ctx.CategoryNames[p.Category],
		VariantGroup: p.VariantGroup,
		Currency:     ctx.Currency,
	}, nil
}

// rssWriter accumulates one RSS-dialect feed. All three RSS kinds share it;
// the kind only namespaces the cache and is the seam for future divergence
// (e.g. TikTok g:video_link).
type rssWriter struct {
	buf bytes.Buffer
}

func newRSSWriter(ctx *feedContext) *rssWriter {
	w := &rssWriter{}
	w.buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	w.buf.WriteString(
		`<rss version="2.0" xmlns:g="http://base.google.com/ns/1.0">` + "\n")
	w.buf.WriteString("  <channel>\n")
	fmt.Fprintf(&w.buf, "    <title>%s</title>\n", escapeXML(ctx.StoreName))
	fmt.Fprintf(&w.buf, "    <link>%s</link>\n",
		escapeXML("https://"+ctx.Domain))
	fmt.Fprintf(&w.buf, "    <description>%s</description>\n",
		escapeXML(ctx.StoreName))
	return w
}

func (w *rssWriter) Item(it *feedItem) {
	fields := make([]string, 0, 12)
	fields = append(fields,
		fmt.Sprintf("<g:id>%d</g:id>", it.ID),
		fmt.Sprintf("<title>%s</title>", escapeXML(it.Title)),
		fmt.Sprintf("<description>%s</description>", escapeXML(it.Description)),
		fmt.Sprintf("<link>%s</link>", escapeXML(it.Link)),
		fmt.Sprintf("<g:image_link>%s</g:image_link>", escapeXML(it.ImageLink)),
		fmt.Sprintf("<g:availability>%s</g:availability>", availability(it.InStock)),
		"<g:condition>new</g:condition>",
		fmt.Sprintf("<g:price>%s</g:price>",
			formatFeedPrice(it.RegularMinor, it.Currency)),
	)
	if it.HasSalePrice {
		fields = append(fields, fmt.Sprintf("<g:sale_price>%s</g:sale_price>",
			formatFeedPrice(it.SaleMinor, it.Currency)))
	}
	fields = append(fields,
		fmt.Sprintf("<g:brand>%s</g:brand>", escapeXML(it.Brand)))
	if it.CategoryName != "" {
		fields = append(fields, fmt.Sprintf(
			"<g:product_type>%s</g:product_type>", escapeXML(it.CategoryName)))
	}
	if it.VariantGroup != nil {
		fields = append(fields, fmt.Sprintf(
			"<g:item_group_id>%d</g:item_group_id>", *it.VariantGroup))
	}
	w.buf.WriteString("    <item>\n      ")
	w.buf.WriteString(strings.Join(fields, "\n      "))
	w.buf.WriteString("\n    </item>\n")
}

func (w *rssWriter) Bytes() []byte {
	out := make([]byte, w.buf.Len(), w.buf.Len()+32)
	copy(out, w.buf.Bytes())
	return append(out, "  </channel>\n</rss>\n"...)
}

func availability(inStock bool) string {
	if inStock {
		return "in stock"
	}
	return "out of stock"
}

func formatFeedPrice(minor int64, currency string) string {
	return money.Format(minor) + " " + currency
}

// escapeXML matches the storefront's escapeXml byte-for-byte (named
// entities including &apos;, which encoding/xml would encode differently).
func escapeXML(v string) string {
	return xmlReplacer.Replace(v)
}

var xmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func stripHTMLTags(v string) string {
	return htmlTagRe.ReplaceAllString(v, "")
}

var (
	hexEntityRe = regexp.MustCompile(`(?i)&#x([0-9a-f]+);`)
	decEntityRe = regexp.MustCompile(`&#(\d+);`)
)

// decodeHTMLEntities resolves entities that survive tag stripping so
// escapeXML doesn't double-encode them into the feed.
func decodeHTMLEntities(v string) string {
	v = hexEntityRe.ReplaceAllStringFunc(v, func(m string) string {
		n, err := strconv.ParseInt(hexEntityRe.FindStringSubmatch(m)[1], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
	v = decEntityRe.ReplaceAllStringFunc(v, func(m string) string {
		n, err := strconv.Atoi(decEntityRe.FindStringSubmatch(m)[1])
		if err != nil {
			return m
		}
		return string(rune(n))
	})
	return strings.NewReplacer(
		"&nbsp;", " ",
		"&quot;", `"`,
		"&apos;", "'",
		"&lt;", "<",
		"&gt;", ">",
		"&amp;", "&",
	).Replace(v)
}

// feedImageURL expands the feed image template, percent-encoding each path
// segment (social crawlers reject raw unicode in URLs). An unresolved
// media origin yields no URL rather than an unreachable one.
func feedImageURL(ctx *feedContext, path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return media.ImageURL(ctx.ImageURLTemplate, ctx.AssetsHost, ctx.Schema,
		strings.Join(segments, "/"))
}

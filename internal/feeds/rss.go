package feeds

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/media"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/text"
)

// The RSS dialect (RSS 2.0 + xmlns:g) is the one Meta Commerce Manager,
// TikTok Ads Manager and Google Merchant Center all ingest, and it is the
// shape those platforms already hold for these stores. Spec constraints:
//   - price format "12.34 EUR" — dot decimal, space, ISO 4217 code
//   - availability enum: "in stock" | "out of stock"
//   - images JPEG/PNG only (never WebP), >=500x500 — the feed image
//     template pins 1000x1000 JPEG on white
//   - g:id equals the pixel/CAPI content_ids — always the product id,
//     never sku/uuid
//   - text limits are the strictest of the three so one document serves
//     them all: Google caps titles at 150 and descriptions at 5000 (Meta
//     allows 200 / 9999; TikTok's catalog spec states no limit)
const (
	feedTitleMax       = 150
	feedDescriptionMax = 5000
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
// reject the product anyway (no default-locale name or no image), so it
// is skipped rather than emitted broken. A malformed money field is an
// error: it would corrupt every consumer's feed.
func newFeedItem(p *django.Product, ctx *feedContext) (*feedItem, error) {
	tr := p.Translations[ctx.Locale]
	if tr.Name == "" || p.MainImagePath == "" {
		return nil, nil
	}
	title := text.Runes(tr.Name, feedTitleMax)
	desc := plainText(tr.Description)
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
		Link: storefront.Product(ctx.Domain, p.ID, p.Slug),
		ImageLink: media.ImageURL(ctx.ImageURLTemplate, ctx.AssetsHost,
			ctx.Schema, p.MainImagePath),
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

// rssWriter accumulates the RSS-dialect feed google.xml, meta.xml and
// tiktok.xml all serve.
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
		escapeXML(storefront.Origin(ctx.Domain)))
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

// escapeXML matches the storefront's escapeXml (named entities including
// &apos;, which encoding/xml would encode differently) and drops runes
// outside the XML 1.0 Char production: one vertical tab pasted from a
// spreadsheet would otherwise make the whole document unparseable, and
// the platforms reject the catalog, not the item.
func escapeXML(v string) string {
	return xmlReplacer.Replace(strings.Map(xmlChar, v))
}

func xmlChar(r rune) rune {
	switch {
	case r == '\t', r == '\n', r == '\r',
		r >= 0x20 && r <= 0xD7FF,
		r >= 0xE000 && r <= 0xFFFD,
		r >= 0x10000 && r <= 0x10FFFF:
		return r
	}
	return -1
}

var xmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

var (
	// Block-level tags separate words; inline ones (<b>, <a>) do not.
	blockTagRe = regexp.MustCompile(`(?i)</?(?:br|p|div|li|ul|ol|h[1-6]|` +
		`tr|td|th|table|blockquote|hr)\b[^>]*>`)
	htmlTagRe = regexp.MustCompile(`<[^>]*>`)
)

// plainText turns a rich-text description into one line of plain text.
// Block tags become spaces so "a<br>b" or "</li><li>" keep their words
// apart; entities are decoded after stripping so an encoded "&lt;b&gt;"
// stays text, and escapeXML encodes the result exactly once.
func plainText(v string) string {
	v = blockTagRe.ReplaceAllString(v, " ")
	v = html.UnescapeString(htmlTagRe.ReplaceAllString(v, ""))
	return strings.Join(strings.Fields(v), " ")
}

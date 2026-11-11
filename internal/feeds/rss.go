package feeds

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

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
//     them all. Google's product data specification
//     (support.google.com/merchants/answer/7052112) is the strictest
//     for every field below; Meta's catalog reference
//     (developers.facebook.com/docs/marketing-api/catalog/reference)
//     allows title 200, description 9999, brand 100, and TikTok's
//     (ads.tiktok.com/help/article/best-practices-for-a-high-quality-catalog)
//     title 150, description 20000.
const (
	feedTitleMax       = 150
	feedDescriptionMax = 5000
	feedBrandMax       = 70
	// Google takes up to 10 additional images per product.
	feedAdditionalImagesMax = 10
	// g:product_detail: name and value, up to 100 per item.
	feedDetailNameMax  = 140
	feedDetailValueMax = 1000
	feedDetailsMax     = 100
	// g:product_highlight: an item with fewer than the minimum breaks its
	// requirements, so it goes out with none.
	feedHighlightsMin     = 2
	feedHighlightsMax     = 100
	feedHighlightMax      = 150
	feedItemGroupTitleMax = 150
	// g:variant_option: a name and a value of up to 250 each, up to 30
	// per item and 5000 characters in all.
	feedVariantOptionPartMax   = 250
	feedVariantOptionsMax      = 30
	feedVariantOptionsTotalMax = 5000
)

// feedContext carries per-generation tenant data every writer needs.
type feedContext struct {
	StoreName string
	// Domain is the storefront host, the channel's link.
	Domain string
	// Links are the storefront's pages in the feed's language.
	Links storefront.Links
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
	// AdditionalImageLinks are the product's other images, in display
	// order after the main one.
	AdditionalImageLinks []string
	InStock              bool
	// Prices are ISO 4217 minor units, like everywhere else in the
	// gateway; formatFeedPrice renders them at the edge. EffectiveMinor
	// is the unit price checkout charges. PriorMinor is the Omnibus
	// reference of an announced reduction and zero when none may be
	// announced: a feed regular price above the charged one is shown to
	// consumers as a reduction, so it may only ever be the prior price.
	EffectiveMinor int64
	PriorMinor     int64
	Brand          string
	Gtin           string
	Mpn            string
	CategoryName   string
	VariantGroup   *int64
	// ItemGroupTitle and VariantOptions describe a variant: the group's
	// name, and the attributes (flagged variant axes) its siblings
	// differ by. Google disapproves items of one group that no
	// attribute tells apart.
	ItemGroupTitle string
	VariantOptions []variantOption
	// Details are the specifications the product page shows: every
	// attribute that is not a variant option above.
	Details []variantOption
	// Highlights are the store's selling points, within Google's bounds.
	Highlights []string
	Currency   string
	// OverCap reports that images, variant options, details or
	// highlights past a target's count limit were left out.
	OverCap bool
}

type variantOption struct {
	Name  string
	Value string
}

// newFeedItem maps a product row; a nil item means the platforms would
// reject the product anyway (no default-locale name or no image), or its
// charged price is not recorded upstream yet, so it is skipped rather
// than emitted broken or at a price checkout would not honour. A
// malformed money field is an error: it would corrupt every consumer's
// feed.
func newFeedItem(p *django.ProductFeed, ctx *feedContext) (*feedItem, error) {
	tr := p.Translations[ctx.Locale]
	if tr.Name == "" || p.MainImagePath == "" || p.EffectivePrice == "" {
		return nil, nil
	}
	title := tr.FeedTitle
	if title == "" {
		title = tr.Name
	}
	title = text.Runes(title, feedTitleMax)
	desc := plainText(tr.Description)
	if desc == "" {
		desc = tr.Name
	}
	desc = text.Runes(desc, feedDescriptionMax)

	effective, err := money.MinorUnits(p.EffectivePrice.String())
	if err != nil {
		return nil, err
	}
	var prior int64
	if p.PriorPrice != "" {
		if prior, err = money.MinorUnits(p.PriorPrice.String()); err != nil {
			return nil, err
		}
	}

	// Options and details go out in the store's order up to their caps;
	// the variant options stop at the first that does not fit, so the
	// ones sent are always the leading ones.
	var options, details []variantOption
	var optionRunes int
	var optionsFull, overCap bool
	for _, a := range p.Attributes {
		if a.AttributeName == "" || a.Value == "" {
			continue
		}
		if a.IsVariant && p.VariantGroup != nil {
			o := variantOption{
				Name:  text.Runes(a.AttributeName, feedVariantOptionPartMax),
				Value: text.Runes(a.Value, feedVariantOptionPartMax),
			}
			n := utf8.RuneCountInString(o.Name) + utf8.RuneCountInString(o.Value)
			optionsFull = optionsFull || len(options) == feedVariantOptionsMax ||
				optionRunes+n > feedVariantOptionsTotalMax
			if optionsFull {
				overCap = true
				continue
			}
			options = append(options, o)
			optionRunes += n
			continue
		}
		if len(details) == feedDetailsMax {
			overCap = true
			continue
		}
		details = append(details, variantOption{
			Name:  text.Runes(a.AttributeName, feedDetailNameMax),
			Value: text.Runes(a.Value, feedDetailValueMax),
		})
	}
	highlights := feedHighlights(p.Highlights)
	overCap = overCap || len(p.Highlights) > feedHighlightsMax
	var groupTitle string
	if p.VariantGroup != nil && p.VariantGroupTitle != nil {
		groupTitle = text.Runes(*p.VariantGroupTitle, feedItemGroupTitleMax)
	}

	var additional []string
	for _, path := range p.ImagePaths {
		if path == p.MainImagePath || path == "" {
			continue
		}
		if len(additional) == feedAdditionalImagesMax {
			overCap = true
			break
		}
		additional = append(additional, media.ImageURL(ctx.ImageURLTemplate,
			ctx.AssetsHost, ctx.Schema, path))
	}

	brand := ctx.StoreName
	if p.BrandName != nil && *p.BrandName != "" {
		brand = *p.BrandName
	}
	brand = text.Runes(brand, feedBrandMax)

	// A product without a category gets no product_type, never the name
	// of whatever category 0 would map to.
	var categoryName string
	if p.Category != nil {
		categoryName = ctx.CategoryNames[*p.Category]
	}

	return &feedItem{
		ID:    p.ID,
		Title: title, Description: desc,
		Link: ctx.Links.Product(p.ID, p.Slug),
		ImageLink: media.ImageURL(ctx.ImageURLTemplate, ctx.AssetsHost,
			ctx.Schema, p.MainImagePath),
		AdditionalImageLinks: additional,
		InStock:              p.Stock > 0,
		EffectiveMinor:       effective,
		PriorMinor:           prior,
		Brand:                brand,
		Gtin:                 p.Gtin,
		Mpn:                  p.Mpn,
		CategoryName:         categoryName,
		VariantGroup:         p.VariantGroup,
		ItemGroupTitle:       groupTitle,
		VariantOptions:       options,
		Details:              details,
		Highlights:           highlights,
		Currency:             ctx.Currency,
		OverCap:              overCap,
	}, nil
}

// feedHighlights keeps the highlights Google accepts for one item.
func feedHighlights(highlights []string) []string {
	if len(highlights) < feedHighlightsMin {
		return nil
	}
	kept := make([]string, 0, min(len(highlights), feedHighlightsMax))
	for _, h := range highlights[:cap(kept)] {
		kept = append(kept, text.Runes(h, feedHighlightMax))
	}
	return kept
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
	)
	for _, link := range it.AdditionalImageLinks {
		fields = append(fields, fmt.Sprintf(
			"<g:additional_image_link>%s</g:additional_image_link>",
			escapeXML(link)))
	}
	fields = append(fields,
		fmt.Sprintf("<g:availability>%s</g:availability>", availability(it.InStock)),
		"<g:condition>new</g:condition>",
	)
	// g:price is the regular price and g:sale_price the current one, shown
	// against it as a reduction; without an announced reduction g:price
	// carries the charged price alone.
	if it.PriorMinor > 0 {
		fields = append(fields,
			fmt.Sprintf("<g:price>%s</g:price>",
				formatFeedPrice(it.PriorMinor, it.Currency)),
			fmt.Sprintf("<g:sale_price>%s</g:sale_price>",
				formatFeedPrice(it.EffectiveMinor, it.Currency)))
	} else {
		fields = append(fields, fmt.Sprintf("<g:price>%s</g:price>",
			formatFeedPrice(it.EffectiveMinor, it.Currency)))
	}
	fields = append(fields,
		fmt.Sprintf("<g:brand>%s</g:brand>", escapeXML(it.Brand)))
	if it.Gtin != "" {
		fields = append(fields,
			fmt.Sprintf("<g:gtin>%s</g:gtin>", escapeXML(it.Gtin)))
	}
	if it.Mpn != "" {
		fields = append(fields,
			fmt.Sprintf("<g:mpn>%s</g:mpn>", escapeXML(it.Mpn)))
	}
	if it.CategoryName != "" {
		fields = append(fields, fmt.Sprintf(
			"<g:product_type>%s</g:product_type>", escapeXML(it.CategoryName)))
	}
	if it.VariantGroup != nil {
		fields = append(fields, fmt.Sprintf(
			"<g:item_group_id>%d</g:item_group_id>", *it.VariantGroup))
		if it.ItemGroupTitle != "" {
			fields = append(fields, fmt.Sprintf(
				"<g:item_group_title>%s</g:item_group_title>",
				escapeXML(it.ItemGroupTitle)))
		}
		for _, o := range it.VariantOptions {
			fields = append(fields, fmt.Sprintf(
				"<g:variant_option><g:name>%s</g:name><g:value>%s</g:value>"+
					"</g:variant_option>", escapeXML(o.Name), escapeXML(o.Value)))
		}
	}
	for _, h := range it.Highlights {
		fields = append(fields, fmt.Sprintf(
			"<g:product_highlight>%s</g:product_highlight>", escapeXML(h)))
	}
	for _, d := range it.Details {
		fields = append(fields, fmt.Sprintf(
			"<g:product_detail><g:attribute_name>%s</g:attribute_name>"+
				"<g:attribute_value>%s</g:attribute_value></g:product_detail>",
			escapeXML(d.Name), escapeXML(d.Value)))
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

// blockTags separate words; inline ones (<b>, <a>) do not.
var blockTags = map[atom.Atom]bool{
	atom.Br: true, atom.P: true, atom.Div: true, atom.Li: true,
	atom.Ul: true, atom.Ol: true, atom.H1: true, atom.H2: true,
	atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Tr: true, atom.Td: true, atom.Th: true, atom.Table: true,
	atom.Blockquote: true, atom.Hr: true,
}

// rawTextTags are the elements the tokenizer reads as raw text, up to
// their end tag (readStartTag in x/net/html/token.go): their content is
// code, a title or a fallback, never the element's text, and markup in
// it comes back as text. TestRawTextTagsMatchTheTokenizer pins the set
// to the tokenizer's behaviour.
var rawTextTags = map[atom.Atom]bool{
	atom.Iframe: true, atom.Noembed: true, atom.Noframes: true,
	atom.Noscript: true, atom.Plaintext: true, atom.Script: true,
	atom.Style: true, atom.Textarea: true, atom.Title: true, atom.Xmp: true,
}

// plainText turns a rich-text description into one line of plain text:
// the text tokens of the HTML tokenizer, so a ">" inside an attribute
// value is never mistaken for a tag's end, and the content of a
// raw-text element is dropped. Block tags become spaces so "a<br>b" or
// "</li><li>" keep their words apart. The tokenizer decodes entities
// once, so an encoded "&lt;b&gt;" stays text, and escapeXML encodes the
// result exactly once.
func plainText(v string) string {
	var b strings.Builder
	var raw atom.Atom
	z := html.NewTokenizer(strings.NewReader(v))
	for {
		switch tt := z.Next(); tt {
		case html.ErrorToken:
			return strings.Join(strings.Fields(b.String()), " ")
		case html.TextToken:
			if raw == 0 {
				b.Write(z.Text())
			}
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := atom.Lookup(name)
			switch {
			case tt == html.StartTagToken && rawTextTags[tag]:
				raw = tag
			case tt == html.EndTagToken && tag == raw:
				raw = 0
			}
			if blockTags[tag] {
				b.WriteByte(' ')
			}
		}
	}
}

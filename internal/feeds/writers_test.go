package feeds

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
)

var update = flag.Bool("update", false, "rewrite golden files")

func goldenCompare(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "golden", name)
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err,
		"golden file missing — regenerate with: go test ./internal/feeds/ -update")
	assert.Equal(t, string(want), string(got))
}

func testFeedContext() *feedContext {
	return &feedContext{
		StoreName:        "Demo Store",
		Domain:           "shop.example.test",
		Links:            storefront.NewSite("el").Links("shop.example.test", "el"),
		AssetsHost:       "assets.platform.test",
		Schema:           "demostore",
		Currency:         "EUR",
		Locale:           "el",
		ImageURLTemplate: "https://{assets_host}/media_stream-image/{path}/1000/1000/contain/center/FFFFFF/5/85.jpeg",
		CategoryNames:    map[int64]string{262: "Ηλεκτρονικά & Gadgets"},
	}
}

func fixtureProducts() []django.ProductFeed {
	rows := make([]django.ProductFeed, 0, 5)
	for _, p := range fixtureCatalogue() {
		rows = append(rows, django.ProductFeed{Product: p})
	}
	return rows
}

func fixtureCatalogue() []django.Product {
	return []django.Product{
		{
			// Plain product, no discount.
			ID: 1,
			Translations: map[string]django.Translation{"el": {
				Name:        "Φορτιστής Κινητού",
				Description: "Γρήγορη φόρτιση &amp; ασφάλεια <b>USB-C</b>.",
			}},
			Slug: "fortistis-kinitou", Category: new(int64(262)),
			Pricing: django.Pricing{EffectivePrice: "464.68"},
			Stock:   186, Active: true,
			MainImagePath: "media/uploads/products/%CF%86%CE%BF%CF%81%CF%84%CE%B9%CF%83%CF%84%CE%AE%CF%82.jpg",
		},
		{
			// Announced reduction, with a brand and item group: the
			// prior price is the regular price, the charged one the sale.
			ID: 2,
			Translations: map[string]django.Translation{"el": {
				Name: "Θήκη Κινητού", Description: "",
			}},
			Slug: "thiki-kinitou", Category: new(int64(262)),
			VariantGroup: new(int64(77)), BrandName: new("Spigen"),
			Pricing: django.Pricing{
				EffectivePrice: "22.32", PriorPrice: "24.80",
				AnnouncedDiscountPercent: "10",
			},
			Stock: 0, Active: true,
			MainImagePath: "media/uploads/products/case.jpg",
			ImagePaths: []string{
				"media/uploads/products/case.jpg",
				"media/uploads/products/case-back.jpg",
				"media/uploads/products/case-side.jpg",
			},
			VariantGroupTitle: new("Θήκη Κινητού"),
			Attributes: []django.ProductAttribute{
				{AttributeName: "Χρώμα", Value: "Μαύρο", IsVariant: true},
				{AttributeName: "Υλικό", Value: "Σιλικόνη"},
			},
		},
		{
			// Reduced (markdown or promotion) but not announceable yet:
			// the charged price alone, no sale price.
			ID: 4,
			Translations: map[string]django.Translation{"el": {
				Name: "Καλώδιο USB-C",
			}},
			Slug: "kalodio-usb-c", Category: new(int64(262)),
			Pricing: django.Pricing{EffectivePrice: "8.37"},
			Stock:   12, Active: true,
			MainImagePath: "media/uploads/products/cable.jpg",
		},
		{
			// No image: the platforms reject it, so the writers skip it.
			ID: 3,
			Translations: map[string]django.Translation{"el": {
				Name: "Αόρατο Προϊόν",
			}},
			Slug: "aorato", Category: new(int64(262)),
			Pricing: django.Pricing{EffectivePrice: "12.40"},
			Stock:   5, Active: true,
			MainImagePath: "",
		},
		{
			// Charged price not recorded upstream yet: skipped, never
			// emitted at a price checkout would not charge.
			ID: 5,
			Translations: map[string]django.Translation{"el": {
				Name: "Νέο Προϊόν",
			}},
			Slug: "neo", Category: new(int64(262)),
			Stock: 5, Active: true,
			MainImagePath: "media/uploads/products/new.jpg",
		},
	}
}

// rssDocument is the RSS document of items.
func rssDocument(t *testing.T, ctx *feedContext, items ...*feedItem) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := newRSSWriter(ctx, &out)
	require.NoError(t, err)
	for _, it := range items {
		require.NoError(t, w.Item(it))
	}
	require.NoError(t, w.Close())
	return out.Bytes()
}

// acpDocument is the ACP document of items.
func acpDocument(t *testing.T, items ...*feedItem) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := newACPWriter(&out)
	require.NoError(t, err)
	for _, it := range items {
		require.NoError(t, w.Item(it))
	}
	require.NoError(t, w.Close())
	return out.Bytes()
}

// fixtureItems are the fixture products a feed lists.
func fixtureItems(t *testing.T, ctx *feedContext) []*feedItem {
	t.Helper()
	var items []*feedItem
	for _, p := range fixtureProducts() {
		it, err := newFeedItem(&p, ctx)
		require.NoError(t, err)
		if it != nil {
			items = append(items, it)
		}
	}
	return items
}

func TestRSSWriterGolden(t *testing.T) {
	ctx := testFeedContext()
	goldenCompare(t, "feed_rss.xml",
		rssDocument(t, ctx, fixtureItems(t, ctx)...))
}

func TestACPWriterGolden(t *testing.T) {
	ctx := testFeedContext()
	got := acpDocument(t, fixtureItems(t, ctx)...)
	// The feed is served compact; the golden file is indented to read.
	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, got, "", " "))
	goldenCompare(t, "feed_acp.json", indented.Bytes())
}

func TestRSSItemRules(t *testing.T) {
	ctx := testFeedContext()
	products := fixtureProducts()

	plain, err := newFeedItem(&products[0], ctx)
	require.NoError(t, err)
	require.NotNil(t, plain)
	// The charged price, VAT-inclusive, in minor units.
	assert.EqualValues(t, 46468, plain.EffectiveMinor)
	assert.Equal(t, "464.68 EUR", formatFeedPrice(plain.EffectiveMinor, "EUR"))
	assert.Zero(t, plain.PriorMinor)
	// HTML is stripped and entities decoded exactly once.
	assert.Equal(t, "Γρήγορη φόρτιση & ασφάλεια USB-C.", plain.Description)
	// Django's path arrives percent-encoded and is linked as it is,
	// never encoded a second time.
	assert.Contains(t, plain.ImageLink,
		"media/uploads/products/%CF%86%CE%BF%CF%81%CF%84%CE%B9%CF%83%CF%84%CE%AE%CF%82.jpg/")
	assert.NotContains(t, plain.ImageLink, "%25")

	sale, err := newFeedItem(&products[1], ctx)
	require.NoError(t, err)
	require.NotNil(t, sale)
	assert.EqualValues(t, 2232, sale.EffectiveMinor)
	assert.EqualValues(t, 2480, sale.PriorMinor)
	assert.Equal(t, "Spigen", sale.Brand)
	assert.False(t, sale.InStock)
	// Empty description falls back to the name.
	assert.Equal(t, "Θήκη Κινητού", sale.Description)
	// A variant names its group and the axis it differs by, never a
	// plain specification; the main image is not repeated.
	assert.Equal(t, "Θήκη Κινητού", sale.ItemGroupTitle)
	assert.Equal(t, []variantOption{{Name: "Χρώμα", Value: "Μαύρο"}},
		sale.VariantOptions)
	// The store's own feed title wins over the name, and its identifiers
	// go out as it entered them.
	identified := products[0]
	tr := identified.Translations["el"]
	tr.FeedTitle = "Φορτιστής USB-C 20W"
	identified.Translations = map[string]django.Translation{"el": tr}
	identified.Gtin, identified.Mpn = "5901234123457", "WS-20"
	it, err := newFeedItem(&identified, ctx)
	require.NoError(t, err)
	assert.Equal(t, "Φορτιστής USB-C 20W", it.Title)
	doc := string(rssDocument(t, ctx, it))
	assert.Contains(t, doc, "<g:gtin>5901234123457</g:gtin>")
	assert.Contains(t, doc, "<g:mpn>WS-20</g:mpn>")

	// Highlights go out only within Google's bounds: one is too few.
	identified.Highlights = []string{"Μόνο ένα"}
	it, err = newFeedItem(&identified, ctx)
	require.NoError(t, err)
	assert.Empty(t, it.Highlights)
	identified.Highlights = []string{"Χωράει στην τσέπη", "Φορτίζει χωρίς καλώδιο"}
	it, err = newFeedItem(&identified, ctx)
	require.NoError(t, err)
	assert.Contains(t, string(rssDocument(t, ctx, it)),
		"<g:product_highlight>Χωράει στην τσέπη</g:product_highlight>")
	identified.Highlights = make([]string, feedHighlightsMax+1)
	it, err = newFeedItem(&identified, ctx)
	require.NoError(t, err)
	assert.Len(t, it.Highlights, feedHighlightsMax)

	// The plain specifications the page shows go out as details.
	assert.Equal(t, []variantOption{{Name: "Υλικό", Value: "Σιλικόνη"}},
		sale.Details)
	require.Len(t, sale.AdditionalImageLinks, 2)
	assert.Contains(t, sale.AdditionalImageLinks[0], "/case-back.jpg/")
	assert.NotContains(t, sale.AdditionalImageLinks[0], "/case.jpg/")

	skipped, err := newFeedItem(&products[3], ctx)
	require.NoError(t, err)
	assert.Nil(t, skipped, "no image -> skipped")

	unpriced, err := newFeedItem(&products[4], ctx)
	require.NoError(t, err)
	assert.Nil(t, unpriced, "no effective price -> skipped")

	for _, mutate := range []func(*django.ProductFeed){
		func(p *django.ProductFeed) { p.EffectivePrice = "not-money" },
		func(p *django.ProductFeed) { p.PriorPrice = "not-money" },
	} {
		bad := products[0]
		mutate(&bad)
		_, err = newFeedItem(&bad, ctx)
		assert.Error(t, err, "malformed money aborts rather than emitting 0")
	}
}

// The feeds price from the upstream Omnibus fields alone. A merchant
// markdown or a promotion without an announceable prior price must not
// surface as a sale, and the price stated is the one checkout charges
// (promotions included), not the deprecated pre-promotion finalPrice.
func TestFeedPricingFollowsOmnibus(t *testing.T) {
	cases := []struct {
		name, payload     string
		price, salePrice  string
		acpPrice, acpList int64
	}{
		{
			name: "announced reduction",
			payload: `"finalPrice":24.90,"discountPercent":20,` +
				`"effectivePrice":24.90,"priorPrice":31.13,` +
				`"announcedDiscountPercent":20`,
			price: "31.13 EUR", salePrice: "24.90 EUR",
			acpPrice: 2490, acpList: 3113,
		},
		{
			name: "markdown not announceable",
			payload: `"finalPrice":24.90,"discountPercent":20,` +
				`"effectivePrice":24.90,"priorPrice":null,` +
				`"announcedDiscountPercent":null`,
			price: "24.90 EUR", acpPrice: 2490,
		},
		{
			name: "automatic promotion lowers the charged price",
			payload: `"finalPrice":30.00,"discountPercent":0,` +
				`"effectivePrice":27.00,"priorPrice":null,` +
				`"announcedDiscountPercent":null`,
			price: "27.00 EUR", acpPrice: 2700,
		},
		{
			name: "announced promotion",
			payload: `"finalPrice":30.00,"discountPercent":0,` +
				`"effectivePrice":27.00,"priorPrice":30.00,` +
				`"announcedDiscountPercent":10`,
			price: "30.00 EUR", salePrice: "27.00 EUR",
			acpPrice: 2700, acpList: 3000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := json.NewDecoder(bytes.NewReader([]byte(
				`{"id":9,"translations":{"el":{"name":"Π"}},"slug":"p",` +
					`"category":262,"stock":1,"active":true,` +
					`"mainImagePath":"m.jpg",` + tc.payload + `}`)))
			dec.UseNumber()
			var p django.ProductFeed
			require.NoError(t, dec.Decode(&p))

			ctx := testFeedContext()
			it, err := newFeedItem(&p, ctx)
			require.NoError(t, err)
			require.NotNil(t, it)

			var doc struct {
				Items []struct {
					Price     string  `xml:"http://base.google.com/ns/1.0 price"`
					SalePrice *string `xml:"http://base.google.com/ns/1.0 sale_price"`
				} `xml:"channel>item"`
			}
			require.NoError(t, xml.Unmarshal(rssDocument(t, ctx, it), &doc))
			require.Len(t, doc.Items, 1)
			assert.Equal(t, tc.price, doc.Items[0].Price)
			if tc.salePrice == "" {
				assert.Nil(t, doc.Items[0].SalePrice)
			} else {
				require.NotNil(t, doc.Items[0].SalePrice)
				assert.Equal(t, tc.salePrice, *doc.Items[0].SalePrice)
			}

			v := acpProductOf(it).Variants[0]
			assert.Equal(t, tc.acpPrice, v.Price.Amount)
			if tc.acpList == 0 {
				assert.Nil(t, v.ListPrice)
			} else {
				require.NotNil(t, v.ListPrice)
				assert.Equal(t, tc.acpList, v.ListPrice.Amount)
			}
		})
	}
}

// The writers hand each part of a document to their writer as it is
// written, never holding the document until it ends.
func TestWritersStreamEachItem(t *testing.T) {
	ctx := testFeedContext()
	items := fixtureItems(t, ctx)
	require.GreaterOrEqual(t, len(items), 2)

	var rssOut bytes.Buffer
	rss, err := newRSSWriter(ctx, &rssOut)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(rssOut.String(), "</description>\n"),
		"the channel's head")
	for i, it := range items {
		require.NoError(t, rss.Item(it))
		assert.Equal(t, i+1, strings.Count(rssOut.String(), "</item>"))
		assert.True(t, strings.HasSuffix(rssOut.String(), "</item>\n"))
	}
	require.NoError(t, rss.Close())
	assert.True(t, strings.HasSuffix(rssOut.String(), "</channel>\n</rss>\n"))

	var acpOut bytes.Buffer
	acp, err := newACPWriter(&acpOut)
	require.NoError(t, err)
	assert.Equal(t, `{"products":[`, acpOut.String())
	require.NoError(t, acp.Item(items[0]))
	first, err := json.Marshal(acpProductOf(items[0]))
	require.NoError(t, err)
	assert.Equal(t, `{"products":[`+string(first), acpOut.String())
	require.NoError(t, acp.Item(items[1]))
	second, err := json.Marshal(acpProductOf(items[1]))
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(acpOut.String(), ","+string(second)))
	require.NoError(t, acp.Close())
	assert.True(t, strings.HasSuffix(acpOut.String(), "]}"))
}

// A store with nothing to list still serves whole documents: a channel
// with no items, and an empty products list the schema accepts.
func TestEmptyCatalogueDocuments(t *testing.T) {
	var rss struct {
		Title string     `xml:"channel>title"`
		Items []struct{} `xml:"channel>item"`
	}
	require.NoError(t, xml.Unmarshal(rssDocument(t, testFeedContext()), &rss))
	assert.NotEmpty(t, rss.Title)
	assert.Empty(t, rss.Items)

	raw := acpDocument(t)
	assert.Equal(t, `{"products":[]}`, string(raw))
	var doc any
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NoError(t, validateACP(t, "ProductsResponse", doc))
}

func TestACPGoldenValidatesAgainstSchema(t *testing.T) {
	// The golden ACP feed must satisfy the vendored spec schema
	// (ProductsResponse) — this is the contract test for the feed shape.
	path := filepath.Join("..", "..", "testdata", "golden", "feed_acp.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc any
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NoError(t, validateACP(t, "ProductsResponse", doc))
}

// Every field stays within the strictest limit of the three targets, cut
// by runes (Greek), and every repeated field within its count.
func TestFeedItemFieldLimits(t *testing.T) {
	ctx := testFeedContext()
	p := fixtureProducts()[1]
	long := func(n int) string { return strings.Repeat("Ω", n) }
	p.BrandName = new(long(255))
	p.VariantGroupTitle = new(long(255))
	p.Highlights = []string{long(200), long(10)}
	p.Attributes = nil
	for i := range 40 {
		p.Attributes = append(p.Attributes, django.ProductAttribute{
			AttributeName: fmt.Sprintf("Ά%d", i), Value: long(255),
			IsVariant: true,
		})
	}
	for i := range 120 {
		p.Attributes = append(p.Attributes, django.ProductAttribute{
			AttributeName: fmt.Sprintf("Λ%d", i), Value: "x",
		})
	}

	it, err := newFeedItem(&p, ctx)
	require.NoError(t, err)
	require.NotNil(t, it)
	runes := utf8.RuneCountInString
	assert.Equal(t, feedBrandMax, runes(it.Brand))
	assert.Equal(t, feedItemGroupTitleMax, runes(it.ItemGroupTitle))
	assert.Equal(t, feedHighlightMax, runes(it.Highlights[0]))
	assert.Len(t, it.Details, feedDetailsMax)

	require.NotEmpty(t, it.VariantOptions)
	assert.LessOrEqual(t, len(it.VariantOptions), feedVariantOptionsMax)
	var total int
	for _, o := range it.VariantOptions {
		assert.LessOrEqual(t, runes(o.Value), feedVariantOptionPartMax)
		total += runes(o.Name) + runes(o.Value)
	}
	assert.LessOrEqual(t, total, feedVariantOptionsTotalMax)
	assert.True(t, utf8.ValidString(it.VariantOptions[0].Value))

	// The count cap applies on its own when the options are short.
	for i := range p.Attributes[:40] {
		p.Attributes[i].Value = "v"
	}
	it, err = newFeedItem(&p, ctx)
	require.NoError(t, err)
	assert.Len(t, it.VariantOptions, feedVariantOptionsMax)
	assert.True(t, it.OverCap)

	// The options sent are the leading ones: one that does not fit ends
	// them, and a shorter one after it is not sent in its place.
	p.Attributes = nil
	for i, n := range []int{240, 240, 240, 240, 240, 240, 240, 240, 240,
		240, 240, 240, 240, 240, 240, 240, 240, 240, 240, 240, 250, 1} {
		p.Attributes = append(p.Attributes, django.ProductAttribute{
			AttributeName: fmt.Sprintf("Ά%02d", i), Value: long(n),
			IsVariant: true,
		})
	}
	it, err = newFeedItem(&p, ctx)
	require.NoError(t, err)
	require.Len(t, it.VariantOptions, 20)
	for i, o := range it.VariantOptions {
		assert.Equal(t, fmt.Sprintf("Ά%02d", i), o.Name)
	}
	assert.True(t, it.OverCap)

	within, err := newFeedItem(&fixtureProducts()[1], ctx)
	require.NoError(t, err)
	assert.False(t, within.OverCap)
}

// The raw-text elements plainText skips are the ones the tokenizer
// reads as raw text, and no other.
func TestRawTextTagsMatchTheTokenizer(t *testing.T) {
	raw := func(name string) bool {
		z := html.NewTokenizer(strings.NewReader("<" + name + "><b>x"))
		require.Equal(t, html.StartTagToken, z.Next(), name)
		return z.Next() == html.TextToken
	}
	for tag := range rawTextTags {
		name := tag.String()
		assert.True(t, raw(name), name)
		// A self-closing start tag enters raw text too, and plainText
		// skips it the same way.
		z := html.NewTokenizer(strings.NewReader("<" + name + "/><b>x"))
		require.Equal(t, html.SelfClosingTagToken, z.Next(), name)
		assert.Equal(t, html.TextToken, z.Next(), name)
		want := "ab"
		if tag == atom.Plaintext {
			want = "a"
		}
		assert.Equal(t, want,
			plainText("a<"+name+"/><p>x</p></"+name+">b"), name)
	}
	for _, name := range []string{
		"a", "abbr", "b", "blockquote", "br", "caption", "code", "col",
		"colgroup", "details", "div", "em", "h1", "h6", "hr", "i", "img",
		"li", "ol", "p", "pre", "s", "section", "span", "strong", "sub",
		"summary", "sup", "table", "tbody", "td", "tfoot", "th", "thead",
		"tr", "u", "ul", "template", "svg", "math", "object", "noindex",
	} {
		assert.False(t, raw(name), name)
	}
}

// Every element x/net/html renders as void (it refuses to render one
// with a child) is in voidTags, and nothing else, among every name the
// atom table holds. A hidden void element hides nothing after it.
func TestVoidTagsCoverTheRenderer(t *testing.T) {
	rendersVoid := func(name string) bool {
		n := &html.Node{Type: html.ElementNode, Data: name}
		n.AppendChild(&html.Node{Type: html.TextNode, Data: "x"})
		return html.Render(io.Discard, n) != nil
	}
	var names int
	for off := atom.Atom(0); (off<<8 | 1).String() != ""; off++ {
		for n := atom.Atom(1); n < 0x100; n++ {
			a := off<<8 | n
			name := a.String()
			if name == "" || atom.Lookup([]byte(name)) != a {
				continue
			}
			names++
			assert.Equal(t, rendersVoid(name), voidTags[a], name)
		}
	}
	require.Greater(t, names, 100, "the atom table was walked")
	for tag := range voidTags {
		name := tag.String()
		assert.Equal(t, "ab", plainText("a<"+name+" hidden>b"), name)
		assert.Equal(t, "ab", plainText("a<"+name+" hidden/>b"), name)
	}
}

func TestPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"a<br>b":                            "a b",
		"<ul><li>one</li><li>two</li></ul>": "one two",
		"<p>x</p><p>y</p>":                  "x y",
		"bold <b>USB-C</b>.":                "bold USB-C.",
		"&lt;b&gt; &amp;amp; &eacute;":      "<b> &amp; é",
		"  spaced\n\tout  ":                 "spaced out",
		// An attribute value may hold ">"; it is never text.
		`<a title="a>b" href="/x">link</a> end`: "link end",
		`<img alt="1 > 0" src="x.jpg">ok`:       "ok",
		"a<script>var x = '<p>';</script>b":     "ab",
		"a<style>p > b { color: red }</style>b": "ab",
		"<!-- note -->text":                     "text",
		// Every raw-text element's content is dropped, markup and all.
		"a<noscript><p>fallback</p></noscript>b":              "ab",
		`a<iframe src="https://x"><p>no frames</p></iframe>b`: "ab",
		"a<title>T</title>b":                                  "ab",
		"a<textarea><b>t</b></textarea>b":                     "ab",
		"a<xmp><i>x</i></xmp>b":                               "ab",
		"a<noembed>e</noembed><noframes>f</noframes>b":        "ab",
		"a<plaintext><p>rest":                                 "a",
		// Django keeps the hidden attribute on any element and the
		// storefront shows nothing of such an element: neither does the
		// feed, nested elements and all.
		"a<span hidden>secret</span>b":                     "ab",
		`a<span hidden="until-found">x</span>b`:            "ab",
		"a<span HIDDEN>x</span>b":                          "ab",
		"a <div hidden><p>x</p><ul><li>y</li></ul></div>b": "a b",
		"<p>a</p><p hidden>x</p><p>b</p>":                  "a b",
		"<div hidden><div>x</div>y</div>z":                 "z",
		"<div hidden><span hidden>x</span>y</div>z":        "z",
		"<div><div hidden>x</div>y</div>z":                 "y z",
		"<span hidden><b>x</b></span>y":                    "y",
		"<div hidden><script>x</div></script>y</div>z":     "z",
		"a<iframe hidden><p>x</p></iframe>b":               "ab",
		// A void element has no content to hide, and a hidden one
		// shows no break either.
		"a<br hidden>b":                   "ab",
		"a<hr hidden/>b":                  "ab",
		`a<img hidden src="x.jpg">b<br>c`: "ab c",
		// The self-closing flag means nothing on a non-void element:
		// it stays open, as a browser reads it.
		"a<span hidden/>x</span>b": "ab",
		// Unclosed, it runs to the end, as a browser reads it.
		"a<span hidden>rest <b>of it": "a",
		// Only the attribute itself hides; Django drops aria-*.
		`a <span data-hidden="1" class="hidden">x</span> b`: "a x b",
		// A literal "<" reaches the gateway as Django stores it: nh3
		// writes "&lt;" (and drops "<y" as a tag on save).
		"a &lt; b": "a < b",
		"x&lt;y z": "x<y z",
		"1 &gt; 0": "1 > 0",
	} {
		assert.Equal(t, want, plainText(in), in)
	}
}

// One control character pasted from a spreadsheet must not make the
// whole document unparseable: the platforms reject the catalog, not the
// item.
func TestRSSDocumentParsesWithHostileText(t *testing.T) {
	ctx := testFeedContext()
	ctx.StoreName = "Demo\x0bStore"
	p := fixtureProducts()[0]
	p.Translations = map[string]django.Translation{"el": {
		Name:        "Form\x0cfeed ￾",
		Description: "vtab &#11; nul &#0; nonchar &#xFFFE; end",
	}}
	it, err := newFeedItem(&p, ctx)
	require.NoError(t, err)
	var doc struct {
		Title string `xml:"channel>title"`
		Items []struct {
			Title       string `xml:"title"`
			Description string `xml:"description"`
		} `xml:"channel>item"`
	}
	require.NoError(t, xml.Unmarshal(rssDocument(t, ctx, it), &doc))
	assert.Equal(t, "DemoStore", doc.Title)
	require.Len(t, doc.Items, 1)
	assert.Equal(t, "Formfeed ", doc.Items[0].Title)
	assert.NotContains(t, doc.Items[0].Description, "\x0b")
}

// A product without a category (Django's SET_NULL) is listed with no
// product_type, even where the store's category names hold an entry 0.
func TestRSSItemWithoutCategory(t *testing.T) {
	ctx := testFeedContext()
	ctx.CategoryNames[0] = "Κατηγορία μηδέν"
	product := fixtureProducts()[0]

	var decoded django.ProductFeed
	require.NoError(t, json.Unmarshal([]byte(`{"category":null}`), &decoded))
	product.Category = decoded.Category
	require.Nil(t, product.Category)

	it, err := newFeedItem(&product, ctx)
	require.NoError(t, err)
	require.NotNil(t, it)
	assert.Empty(t, it.CategoryName)

	assert.NotContains(t, string(rssDocument(t, ctx, it)), "<g:product_type>")

	categorised, err := newFeedItem(&fixtureProducts()[0], ctx)
	require.NoError(t, err)
	assert.Equal(t, "Ηλεκτρονικά & Gadgets", categorised.CategoryName)
}

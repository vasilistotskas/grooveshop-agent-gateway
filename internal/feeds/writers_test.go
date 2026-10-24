package feeds

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
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
		AssetsHost:       "assets.platform.test",
		Schema:           "demostore",
		Currency:         "EUR",
		Locale:           "el",
		ImageURLTemplate: "https://{assets_host}/media_stream-image/{path}/1000/1000/contain/center/FFFFFF/5/85.jpeg",
		CategoryNames:    map[int64]string{262: "Ηλεκτρονικά & Gadgets"},
	}
}

func fixtureProducts() []django.Product {
	return []django.Product{
		{
			// Plain product, no discount.
			ID: 1,
			Translations: map[string]django.Translation{"el": {
				Name:        "Φορτιστής Κινητού",
				Description: "Γρήγορη φόρτιση &amp; ασφάλεια <b>USB-C</b>.",
			}},
			Slug: "fortistis-kinitou", Category: 262,
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
			Slug: "thiki-kinitou", Category: 262,
			VariantGroup: new(int64(77)), BrandName: new("Spigen"),
			Pricing: django.Pricing{
				EffectivePrice: "22.32", PriorPrice: "24.80",
				AnnouncedDiscountPercent: "10",
			},
			Stock: 0, Active: true,
			MainImagePath: "media/uploads/products/case.jpg",
		},
		{
			// Reduced (markdown or promotion) but not announceable yet:
			// the charged price alone, no sale price.
			ID: 4,
			Translations: map[string]django.Translation{"el": {
				Name: "Καλώδιο USB-C",
			}},
			Slug: "kalodio-usb-c", Category: 262,
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
			Slug: "aorato", Category: 262,
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
			Slug: "neo", Category: 262,
			Stock: 5, Active: true,
			MainImagePath: "media/uploads/products/new.jpg",
		},
	}
}

func TestRSSWriterGolden(t *testing.T) {
	ctx := testFeedContext()
	w := newRSSWriter(ctx)
	for _, p := range fixtureProducts() {
		it, err := newFeedItem(&p, ctx)
		require.NoError(t, err)
		if it != nil {
			w.Item(it)
		}
	}
	goldenCompare(t, "feed_rss.xml", w.Bytes())
}

func TestACPWriterGolden(t *testing.T) {
	ctx := testFeedContext()
	w := newACPWriter()
	for _, p := range fixtureProducts() {
		it, err := newFeedItem(&p, ctx)
		require.NoError(t, err)
		if it != nil {
			w.Item(it)
		}
	}
	got, err := w.Bytes()
	require.NoError(t, err)
	goldenCompare(t, "feed_acp.json", got)
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

	skipped, err := newFeedItem(&products[3], ctx)
	require.NoError(t, err)
	assert.Nil(t, skipped, "no image -> skipped")

	unpriced, err := newFeedItem(&products[4], ctx)
	require.NoError(t, err)
	assert.Nil(t, unpriced, "no effective price -> skipped")

	for _, mutate := range []func(*django.Product){
		func(p *django.Product) { p.EffectivePrice = "not-money" },
		func(p *django.Product) { p.PriorPrice = "not-money" },
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
			var p django.Product
			require.NoError(t, dec.Decode(&p))

			ctx := testFeedContext()
			it, err := newFeedItem(&p, ctx)
			require.NoError(t, err)
			require.NotNil(t, it)

			rss := newRSSWriter(ctx)
			rss.Item(it)
			var doc struct {
				Items []struct {
					Price     string  `xml:"http://base.google.com/ns/1.0 price"`
					SalePrice *string `xml:"http://base.google.com/ns/1.0 sale_price"`
				} `xml:"channel>item"`
			}
			require.NoError(t, xml.Unmarshal(rss.Bytes(), &doc))
			require.Len(t, doc.Items, 1)
			assert.Equal(t, tc.price, doc.Items[0].Price)
			if tc.salePrice == "" {
				assert.Nil(t, doc.Items[0].SalePrice)
			} else {
				require.NotNil(t, doc.Items[0].SalePrice)
				assert.Equal(t, tc.salePrice, *doc.Items[0].SalePrice)
			}

			acp := newACPWriter()
			acp.Item(it)
			v := acp.feed.Products[0].Variants[0]
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

func TestPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"a<br>b":                            "a b",
		"<ul><li>one</li><li>two</li></ul>": "one two",
		"<p>x</p><p>y</p>":                  "x y",
		"bold <b>USB-C</b>.":                "bold USB-C.",
		"&lt;b&gt; &amp;amp; &eacute;":      "<b> &amp; é",
		"  spaced\n\tout  ":                 "spaced out",
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
	w := newRSSWriter(ctx)
	w.Item(it)

	var doc struct {
		Title string `xml:"channel>title"`
		Items []struct {
			Title       string `xml:"title"`
			Description string `xml:"description"`
		} `xml:"channel>item"`
	}
	require.NoError(t, xml.Unmarshal(w.Bytes(), &doc))
	assert.Equal(t, "DemoStore", doc.Title)
	require.Len(t, doc.Items, 1)
	assert.Equal(t, "Formfeed ", doc.Items[0].Title)
	assert.NotContains(t, doc.Items[0].Description, "\x0b")
}

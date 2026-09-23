package feeds

import (
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
			Price: "387.23", VatValue: "77.45", FinalPrice: "464.68",
			DiscountPercent: "0.0", Stock: 186, Active: true,
			MainImagePath: "media/uploads/products/φορτιστής.jpg",
		},
		{
			// Discounted variant with a brand and item group.
			ID: 2,
			Translations: map[string]django.Translation{"el": {
				Name: "Θήκη Κινητού", Description: "",
			}},
			Slug: "thiki-kinitou", Category: 262,
			VariantGroup: new(int64(77)), BrandName: new("Spigen"),
			Price: "20.00", VatValue: "4.80", FinalPrice: "22.32",
			DiscountPercent: "10.0", Stock: 0, Active: true,
			MainImagePath: "media/uploads/products/case.jpg",
		},
		{
			// No image: the platforms reject it, so the writers skip it.
			ID: 3,
			Translations: map[string]django.Translation{"el": {
				Name: "Αόρατο Προϊόν",
			}},
			Slug: "aorato", Category: 262,
			Price: "10.00", VatValue: "2.40", FinalPrice: "12.40",
			DiscountPercent: "0.0", Stock: 5, Active: true,
			MainImagePath: "",
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
	// Regular price is VAT-inclusive (price + vatValue), in minor units.
	assert.EqualValues(t, 46468, plain.RegularMinor)
	assert.Equal(t, "464.68 EUR", formatFeedPrice(plain.RegularMinor, "EUR"))
	assert.False(t, plain.HasSalePrice)
	// HTML is stripped and entities decoded exactly once.
	assert.Equal(t, "Γρήγορη φόρτιση & ασφάλεια USB-C.", plain.Description)
	// Unicode path segments are percent-encoded, separators kept.
	assert.Contains(t, plain.ImageLink,
		"media/uploads/products/%CF%86%CE%BF%CF%81%CF%84%CE%B9%CF%83%CF%84%CE%AE%CF%82.jpg")

	sale, err := newFeedItem(&products[1], ctx)
	require.NoError(t, err)
	require.NotNil(t, sale)
	assert.True(t, sale.HasSalePrice)
	assert.Equal(t, "Spigen", sale.Brand)
	assert.False(t, sale.InStock)
	// Empty description falls back to the name.
	assert.Equal(t, "Θήκη Κινητού", sale.Description)

	skipped, err := newFeedItem(&products[2], ctx)
	require.NoError(t, err)
	assert.Nil(t, skipped, "no image -> skipped")

	bad := products[0]
	bad.Price = "not-money"
	_, err = newFeedItem(&bad, ctx)
	assert.Error(t, err, "malformed money aborts rather than emitting 0")
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

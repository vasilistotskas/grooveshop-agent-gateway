package chat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcpsrv"
)

func structured(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func ids(ev *productsEvent) []int64 {
	out := make([]int64, 0, len(ev.Products))
	for _, p := range ev.Products {
		out = append(out, p.ID)
	}
	return out
}

func searchOut(total int64, productIDs ...int64) mcpsrv.SearchProductsOut {
	out := mcpsrv.SearchProductsOut{TotalHits: total}
	for _, id := range productIDs {
		out.Products = append(out.Products, mcpsrv.ProductSummary{
			ID: id, Name: "p", FinalPrice: "9.90",
		})
	}
	return out
}

func TestRecordProductsSearch(t *testing.T) {
	b := &bridge{}
	ev := b.recordProducts("search_products",
		map[string]any{"query": "θήκη"},
		structured(t, searchOut(123, 510, 7141)))

	require.NotNil(t, ev)
	assert.Equal(t, "search_products", ev.Tool)
	assert.Equal(t, "θήκη", ev.Query)
	require.NotNil(t, ev.Total)
	assert.Equal(t, int64(123), *ev.Total)
	assert.Equal(t, []int64{510, 7141}, ids(ev))

	// The wire shape the widget parses: identities only, no prices.
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	assert.JSONEq(t, `{"tool":"search_products","query":"θήκη",`+
		`"total":123,"products":[{"id":510},{"id":7141}]}`, string(raw))
}

func TestRecordProductsDetailIncludesVariants(t *testing.T) {
	b := &bridge{}
	out := mcpsrv.GetProductOut{
		ProductSummary: mcpsrv.ProductSummary{ID: 1},
		Variants: []mcpsrv.ProductSummary{
			{ID: 2}, {ID: 3},
		},
	}
	ev := b.recordProducts("get_product",
		map[string]any{"productId": 1}, structured(t, out))

	require.NotNil(t, ev)
	assert.Equal(t, "get_product", ev.Tool)
	assert.Empty(t, ev.Query)
	assert.Nil(t, ev.Total, "a detail result reports no total")
	assert.Equal(t, []int64{1, 2, 3}, ids(ev))

	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	assert.JSONEq(t, `{"tool":"get_product",`+
		`"products":[{"id":1},{"id":2},{"id":3}]}`, string(raw))
}

// An empty search still reports what was searched, so the widget can say
// "no matches" rather than nothing.
func TestRecordProductsEmptySearchKeepsTotal(t *testing.T) {
	b := &bridge{}
	ev := b.recordProducts("search_products",
		map[string]any{"query": "τίποτα"}, structured(t, searchOut(0)))

	require.NotNil(t, ev)
	require.NotNil(t, ev.Total)
	assert.Zero(t, *ev.Total)
	assert.Empty(t, ev.Products)
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"products":[]`,
		"an empty list, never null")
}

func TestRecordProductsIgnoresNonProductAndMalformed(t *testing.T) {
	cases := map[string]struct {
		tool string
		raw  []byte
	}{
		"non-product tool": {
			"list_categories", []byte(`{"categories":[{"id":4}]}`),
		},
		"cart tool": {
			"add_to_cart", []byte(`{"items":[{"productId":4}]}`),
		},
		"search not json": {"search_products", []byte(`not json`)},
		"search wrong shape": {
			"search_products", []byte(`{"products":"nope"}`),
		},
		"detail not an object": {"get_product", []byte(`[1,2]`)},
		"detail without an id": {"get_product", []byte(`{}`)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := &bridge{}
			assert.Nil(t, b.recordProducts(tc.tool, nil, tc.raw))
		})
	}
}

func TestRecordProductsDedupesAcrossTheTurn(t *testing.T) {
	b := &bridge{}
	first := b.recordProducts("search_products",
		map[string]any{"query": "a"}, structured(t, searchOut(3, 1, 2, 1)))
	require.NotNil(t, first)
	assert.Equal(t, []int64{1, 2}, ids(first), "duplicates in one result")

	detail := b.recordProducts("get_product", nil, structured(t,
		mcpsrv.GetProductOut{
			ProductSummary: mcpsrv.ProductSummary{ID: 2},
			Variants:       []mcpsrv.ProductSummary{{ID: 3}},
		}))
	require.NotNil(t, detail)
	assert.Equal(t, []int64{3}, ids(detail))

	assert.Nil(t, b.recordProducts("get_product", nil, structured(t,
		mcpsrv.GetProductOut{ProductSummary: mcpsrv.ProductSummary{ID: 1}})),
		"a detail naming only products already sent says nothing")

	again := b.recordProducts("search_products",
		map[string]any{"query": "a"}, structured(t, searchOut(3, 1, 2)))
	require.NotNil(t, again, "a search keeps its total")
	assert.Empty(t, again.Products)
}

func TestRecordProductsCapsInOrder(t *testing.T) {
	b := &bridge{}
	many := make([]int64, 0, maxProductCards+4)
	for id := range int64(maxProductCards + 4) {
		many = append(many, id+100)
	}
	ev := b.recordProducts("search_products",
		map[string]any{"query": "x"}, structured(t, searchOut(500, many...)))

	require.NotNil(t, ev)
	assert.Equal(t, many[:maxProductCards], ids(ev))
	assert.Equal(t, int64(500), *ev.Total)

	// What the cap cut was never shown, so a later call may show it.
	rest := b.recordProducts("search_products",
		map[string]any{"query": "x"}, structured(t, searchOut(500, many...)))
	require.NotNil(t, rest)
	assert.Equal(t, many[maxProductCards:], ids(rest))
}

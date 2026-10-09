package chat

import (
	"encoding/json"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcpsrv"
)

// maxProductCards bounds one products event: the chat panel shows a
// handful of cards, and each one costs the storefront a product fetch.
const maxProductCards = 6

type productRef struct {
	ID int64 `json:"id"`
}

// productsEvent names the products a tool result returned, so the widget
// can render cards. Identities only: the storefront loads each product
// with the shopper's own session, so prices (B2B group pricing included)
// and stock are the shopper's real ones, never the retail figures the
// gateway's anonymous catalog calls see.
type productsEvent struct {
	Tool     string       `json:"tool"`
	Query    string       `json:"query,omitempty"`
	Total    *int64       `json:"total,omitempty"`
	Products []productRef `json:"products"`
}

// productExtractor decodes a successful tool's structured result into the
// event's tool-specific fields and the ids it names, in result order.
type productExtractor func(
	input map[string]any, structured []byte,
) (productsEvent, []int64, bool)

// productExtractors cover the catalog tools. Cart and alert tools
// reference products too, but the widget already shows the cart, and an
// alert confirmation is not a recommendation.
var productExtractors = map[string]productExtractor{
	"search_products": func(
		input map[string]any, structured []byte,
	) (productsEvent, []int64, bool) {
		var out mcpsrv.SearchProductsOut
		if err := json.Unmarshal(structured, &out); err != nil {
			return productsEvent{}, nil, false
		}
		ids := make([]int64, 0, len(out.Products))
		for _, p := range out.Products {
			ids = append(ids, p.ID)
		}
		query, _ := input["query"].(string)
		return productsEvent{Query: query, Total: &out.TotalHits}, ids, true
	},
	"get_product": func(
		_ map[string]any, structured []byte,
	) (productsEvent, []int64, bool) {
		var out mcpsrv.GetProductOut
		if err := json.Unmarshal(structured, &out); err != nil {
			return productsEvent{}, nil, false
		}
		// Variants are sibling products (another colour, another size):
		// "do you have it in red?" is answered by one of them.
		ids := []int64{out.ID}
		for _, v := range out.Variants {
			ids = append(ids, v.ID)
		}
		return productsEvent{}, ids, true
	},
}

// recordProducts builds the products event for one successful tool call,
// dropping ids already sent this turn and capping the list. It returns nil
// for tools that return no products, for a result that does not decode,
// and when nothing is left to say — a search keeps its event even with no
// new products, because its total still reports what was searched.
func (b *bridge) recordProducts(
	tool string, input map[string]any, structured []byte,
) *productsEvent {
	extract, ok := productExtractors[tool]
	if !ok {
		return nil
	}
	ev, ids, ok := extract(input, structured)
	if !ok {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.productsSent == nil {
		b.productsSent = map[int64]bool{}
	}
	ev.Tool = tool
	ev.Products = []productRef{}
	for _, id := range ids {
		if len(ev.Products) == maxProductCards {
			break
		}
		if id <= 0 || b.productsSent[id] {
			continue
		}
		b.productsSent[id] = true
		ev.Products = append(ev.Products, productRef{ID: id})
	}
	if len(ev.Products) == 0 && ev.Total == nil {
		return nil
	}
	return &ev
}

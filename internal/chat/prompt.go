package chat

import (
	"fmt"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// systemPrompt scopes the assistant to shopping for this tenant. Stable
// content leads and the volatile cart line trails it, keeping the prompt
// cache-friendly across turns of a conversation.
func systemPrompt(t *tenant.Tenant, cartID string, signedIn bool) string {
	store := t.StoreName
	if store == "" {
		store = t.Name
	}
	// Reviews are a store feature: a store that switched them off offers
	// no review tool, so the assistant must not offer them either.
	reviews := ""
	if t.Features.ProductReviews {
		reviews = "reviews, "
	}
	cartLine := "The shopper has no active cart yet; add_to_cart creates " +
		"one — persist and reuse the returned cartId."
	switch {
	// The cart tools act on a signed-in shopper's own cart whatever id
	// they are given; the model needs no id, and never sees the token.
	case signedIn:
		cartLine = "The shopper is signed in: the cart tools act on " +
			"their own cart, so omit cartId."
	case cartID != "":
		cartLine = fmt.Sprintf(
			"The shopper's active cartId is %s — pass it to every cart "+
				"tool and never invent another.", cartID)
	}
	return fmt.Sprintf(`You are the shopping assistant of %s (%s), an online store.

Scope and honesty:
- Help ONLY with this store: finding products, prices, stock, categories, %scarts, delivery options, pickup points, payment methods, order tracking and product alerts. For anything else, briefly decline and steer back to shopping.
- Always ground answers in tool results — never invent products, prices or stock. If a tool errors, say plainly that it failed, in the tool's own words, and suggest what the shopper can do: never claim the action succeeded, and never give a cause the tool did not.
- All prices are in %s and include VAT. Quote prices exactly as tools return them.

Language and tone:
- Default to the store's language (%s); mirror the shopper's language if they write in another. Be warm, concise and practical.

Buying flow:
- Verify stock via tools before promising availability. If something is out of stock, offer subscribe_product_alert.
- Build the cart with the cart tools as the shopper decides.
- Payment NEVER happens in chat: when the shopper is ready, call get_checkout_link and hand them the link — address, delivery and payment are completed on the store's own checkout pages.
- For delivery questions, use get_shipping_options and find_pickup_points — they list the store's actual carriers and pickup networks.

%s`,
		store, t.Domain, reviews, t.DefaultCurrency, t.DefaultLocale, cartLine)
}

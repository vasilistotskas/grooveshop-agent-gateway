---
paths:
  - "internal/chat/**"
  - "internal/mcpsrv/turn.go"
  - "internal/mcpsrv/cart_activity.go"
  - "internal/mcpsrv/shopper_attribution.go"
---

# Chat SSE contract (`/chat`)

Moved from `CLAUDE.md`; loads when working on the chat. The consumer is the
storefront's `app/composables/useShopChat.ts`, so a change here is a
cross-repo change. The code wins over this page.

- Nuxt chat widget (`app/composables/useShopChat.ts`) reads the `/chat`
  SSE events: `delta {text}`, `tool {name, status: running|done|error}`
  (`error`: the call failed, and the model was told why),
  `products {tool, query?, total?, products: [{id}]}`,
  `error {message, code?, retryAfter?}` (`code: rate_limited` when the
  model provider throttled the turn, `retryAfter` its wait in seconds),
  `done {conversationId, cartId?, cartMutated, cartTokenRefused}`.
  The request is `{message, conversationId?, cartId?, cartToken?,
  turnId?, locale?, attribution?}`. `attribution` is the tab's first
  touch exactly as the storefront's `useOrderAttribution().read()`
  returns it: `{utmSource?, utmMedium?, utmCampaign?, clickIds?,
  referrer?, landingPath?}` (`chat.chatAttribution`, Django's
  `OrderAttributionInputRequest` without `agentProtocol`). A wrong type
  is a 400; content is Django's to judge. It rides the turn's context
  (`mcpsrv.WithShopperAttribution`), never the stored conversation, and
  every `add_to_cart` of the turn sends it on `POST /cart/item`, so the
  cart the assistant first fills records the visit's source. There is no
  field for `agentProtocol`: Django believes it beside the internal
  secret every gateway cart call carries, so a shopper must never be able
  to set it. Without `attribution` the add sends none. `locale` is the storefront page's language code
  (`el`, `en`, …): the turn is served in it when the store serves it
  (`TenantConfig.ServedLocales`: `availableLocales`, else the default
  alone), else in the store's default; an unserved one is ignored, not
  refused. The turn's language rides the request context
  (`django.WithLanguage`) like the cart token, and every tool handler
  reads it through `django.Language` — `X-Language`, the search's
  `languageCode` and the translation picked — so product names come
  back in the page's language. `/mcp` names none and stays on the
  store's default. The refusals and SSE `error` messages speak it, and
  the prompt tells the model to reply in the language of the shopper's
  latest message, else in the turn's, named in English
  (`chat.languageName`). The prompt also forbids naming a product,
  category or brand no tool result showed, and answers return and
  policy questions only from `get_store_policies`. `turnId` is a UUID the widget mints per message and sends
  again when the shopper retries it: each `add_to_cart` of the turn
  sends an `Idempotency-Key` derived from it, the call and how many
  identical adds the turn already made (`mcpsrv.Turn`), so Django's
  idempotency middleware answers a retried add with the first outcome
  instead of adding twice. Django scopes the key to the caller, which
  `django.RelayCaller` names, so a retry on the other pod is the same
  caller;
  `cartToken` is a signed-in shopper's pass from Django's
  `POST /cart/agent-token`. With it the cart tools act on that shopper's
  cart whatever `cartId` the model names, and `get_checkout_link` links
  `/checkout` instead of the cart claim. It lives in the turn's context
  only: never the stored conversation, the prompt or a log.
  `cartTokenRefused: true` means Django refused it on a call this turn
  (expired, revoked), and the widget mints a new token. It does not mean
  nothing changed: a change Django accepted before a refused read-back
  still counts, so `cartMutated` and `cartTokenRefused` can both be true.
  `done.cartId` is sent on anonymous turns only, and the widget adopts
  it only then: a turn that sent a `cartToken` gets none, and the
  widget ignores `done.cartId` whenever it sent one, refreshing the
  shopper's own cart through their session on `cartMutated`.
  `cartMutated` is recorded by the cart tools when Django accepts a
  change (`mcpsrv.CartActivity`), so a change whose read-back failed
  still counts. The chat runs on its own server (`mcpsrv.NewChatServer`):
  - the cart tools' `cartId` is optional there (`cartToolOf`), while
    `/mcp` requires it;
  - only the chat carries the `chatOnly` tools: `apply_coupon` and
    `remove_coupon`, gated on `promotions`, never on `/mcp` or in
    discovery. `products` follows the
  `tool` done event of `search_products` (query + `totalHits` as total)
  and `get_product` (the product, then its variants), decoded from the
  tool's structured result (`internal/chat/products.go`), deduplicated
  across the turn and capped per event. Ids only, never prices: the
  storefront loads each card with the shopper's session, which is the
  only place B2B pricing applies (see the B2B note in `CLAUDE.md`).

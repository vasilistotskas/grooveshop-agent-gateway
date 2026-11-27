---
paths:
  - "internal/chat/**"
  - "internal/mcpsrv/turn.go"
  - "internal/mcpsrv/cart_activity.go"
  - "internal/mcpsrv/shopper_attribution.go"
  - "internal/mcpsrv/server.go"
  - "internal/mcpsrv/cart.go"
  - "internal/django/language.go"
  - "internal/django/caller.go"
  - "internal/django/cart_token.go"
---

# Chat SSE contract (`/chat`)

Loads when working on the chat. The consumer is the
storefront's `app/composables/useShopChat.ts`, so a change here is a
cross-repo change. The code wins over this page.

- Nuxt chat widget (`app/composables/useShopChat.ts`) reads the `/chat`
  SSE events: `delta {text}`, `tool {name, status: running|done|error}`
  (`error`: the call failed, and the model was told why),
  `products {tool, query?, total?, products: [{id}]}`,
  `error {message, code?, retryAfter?, conversationId?, cartId?,
  cartMutated, cartTokenRefused}` (`code: rate_limited` when the
  model provider throttled the turn, with a 429 or an error inside its
  stream, `retryAfter` its wait in seconds; a turn past
  `CHAT_TURN_TIMEOUT`, or still running when the pod's shutdown drain
  ends, gets the `turn_failed` message with no code, and the widget's
  retry, on another pod after a shutdown, resends it),
  `done {conversationId, cartId?, cartMutated, cartTokenRefused}`.
  - **A turn that fails says what `done` would about the cart.** The
    store keeps a cart change it accepted whatever became of the turn,
    so `error` carries `cartId?`, `cartMutated` and `cartTokenRefused`
    with `done`'s meaning and rules (`chat.turnCart`, one struct in both
    events), however the turn failed: a provider error or throttle, the
    turn's deadline, the pod's shutdown (`Service.endFailed`). The
    widget acts on them as on `done`: it drops a refused cart token, and
    on `cartMutated` it claims `cartId` (a visitor's turn that sent no
    cart) or refreshes the cart. A widget that reads only `message`,
    `code` and `retryAfter` ignores the rest.
  - **A failed turn stores its conversation only for its cart.** When
    a visitor's cart tool named a cart, the conversation is stored with
    it and `error.conversationId` names the conversation, for the widget
    to go on with; it is named only once it is stored. A signed-in turn
    whose stored conversation still named a guest cart stores it without
    that cart, as a finished one does. None of the failed turn's messages are stored: the
    retry sends them again. So a later turn of that conversation that
    sends no `cartId` is told to use that cart and creates no other,
    also after a shopper pressed stop or left, when nobody reads an
    event. A first turn's widget that read no event holds no
    conversation id, so its next turn starts another conversation and
    that cart stays unclaimed. Any other failed
    turn stores nothing and names no conversation. The save runs on a
    context the shopper's hang-up and the shutdown cannot end
    (`Service.save`), before the event, so the event never names a
    conversation that is not stored. It ends at the Redis client's own
    timeouts, well inside the shutdown's final window.
  - The cart members are what the turn's tools had recorded when it
    ended. A cart call still running when the shopper leaves or the
    shutdown cuts the turn is not in them, though the store may accept
    it (an add is sent to its end). The retry with the same `turnId`
    reports it for a cart the shopper has (a sent `cartId`, a
    `cartToken`); a cart that call created for a visitor without one
    stays unclaimed.

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
  store's default. The refusals and SSE `error` messages speak the
  turn's language, and the prompt tells the model to reply in the
  language of the shopper's latest message, else in the turn's, named in
  English (`chat.languageName`). A placed order's lines are the
  exception to the turn's language: they show the name the line was sold
  under, in the order's language (`OrderItem.ProductName`), never the
  live product's name. `track_order` shows the SKU it was sold under too
  (`OrderItem.SKU`); the UCP/ACP order and completed-checkout lines carry
  the name only. The prompt also forbids naming a product,
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
  `cartId`, on `done` and on `error`, is sent on anonymous turns only,
  and the widget adopts it only then: a turn that sent a `cartToken`
  gets none, and the widget ignores `cartId` whenever it sent one,
  refreshing the shopper's own cart through their session on
  `cartMutated`. A turn that sent a `cartToken` keeps no cart in the
  stored conversation either: the id a tool result names is not stored,
  and one an earlier anonymous turn stored is dropped (a sign-in merges
  that guest cart away). The widget keeps its conversation across a
  sign-out, so the next turn comes without a token, and a signed-in
  cart's id named to it would be one Django serves to its owner only
  (an anonymous call naming it is served a new guest cart,
  `django.ErrCartUnreachable`). A cart the
  model created while the widget sent one would never reach the shopper,
  so a sent cart the store no longer serves is answered `cart_unreachable`
  with "reload the page and try again", never "create a new one"
  (`handlers.cartErr`): a reload has the storefront fetch the shopper's
  cart again, a new one for a deleted guest cart, and the widget sends
  that.
  `cartMutated` is recorded by the cart tools when Django accepts a
  change (`mcpsrv.CartActivity`), so a change whose read-back failed
  still counts. The chat runs on its own server (`mcpsrv.NewChatServer`):
  - the cart tools' `cartId` is optional there (`cartToolOf`), while
    `/mcp` requires it;
  - only the chat carries the `chatOnly` tools: `apply_coupon` and
    `remove_coupon`, gated on `promotions`, never on `/mcp` or in
    discovery;
  - it never registers /mcp's own: the six UCP tools (`meta`), the
    account tools (a linked bearer) and `create_cart` (`add_to_cart`
    without a cart creates one, which the widget adopts);
  - `done.cartId` is read from the result of a tool marked
    `changesCart` (`mcpsrv.ChangesCart`).

  `products` follows the
  `tool` done event of `search_products` (query + `totalHits` as total)
  and `get_product` (the product, then its variants), decoded from the
  tool's structured result (`internal/chat/products.go`), deduplicated
  across the turn and capped per event. Ids only, never prices: the
  storefront loads each card with the shopper's session, which is the
  only place B2B pricing applies (see the B2B note in `AGENTS.md`).

## Spend

Chat spend is bounded in three layers:

- Traefik's `rate-limit-chat`, per IP.
- The gateway's per-pod `(host, client)` limiter (`CHAT_RATE_LIMIT_*`).
- The Redis turn quota in `internal/chat/quota.go`, shared by every pod and counted only for valid turns, before any model call:
  - `CHAT_STORE_TURNS_PER_HOUR` per store per clock hour;
  - `CHAT_VISITOR_TURNS_PER_DAY` per client per store per UTC day.

The gateway's "client" is `httpmw.ClientBucket`, the one key both use: an IPv4 address (IPv4-mapped IPv6 included), or an IPv6 address's /64.

A refused turn spends from neither budget and gets the store's localized `rate_limited` message with a 429 and a `Retry-After` until the full window resets.

Inside a turn (`chat.runTurn`):

- At most `CHAT_MAX_ITERATIONS` model calls. The last one is sent `tool_choice: none`, and tool calls a provider returns there anyway are not run.
- The model client is built once (`chat.New`) with no SDK retries. A provider throttle, answered as a 429 or sent as an error inside the stream (`chat.providerErrorOf`), ends the turn after one call with SSE `error {code: rate_limited, retryAfter}`.
- `CHAT_TURN_TIMEOUT`, counted from the turn's start, bounds the model calls and when a tool call may start. Once it has passed, no further tool call of the batch starts, and the turn ends with the generic, retryable `error {message}`; a stalled provider ends the same way. A tool call already running is never cut, so a cart call is not cut midway. A turn that fails after a cart change Django accepted says so: its `error` event carries `done`'s cart members (`cartMutated`, `cartId` for a visitor, `cartTokenRefused`), and a visitor's cart the turn filled is kept in the stored conversation, which `error.conversationId` names (`Service.endFailed`; above). Those members are what the turn's tools had recorded when it ended: a cart call still running when the shopper leaves or the shutdown cuts the turn is not in them, though the store may accept it, so `cartMutated: false` does not prove the cart is unchanged. A retry with the same `turnId` does not add a product twice: `add_to_cart` sends an `Idempotency-Key` derived from the turn (`mcpsrv.Turn`), which Django's `core/middleware/idempotency.py` answers with the first outcome.
- A shutdown cancels the deadline's context too, and `handle` checks for it (`httpmw.ErrShuttingDown`) before anything else, so a turn cut by shutdown is reported as one.

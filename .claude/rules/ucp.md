---
paths:
  - "internal/ucp/**"
  - "internal/mcpsrv/ucp_*.go"
  - "internal/mcpsrv/order.go"
  - "internal/httpsig/**"
  - "internal/checkout/**"
  - "internal/chat/bridge.go"
---

# UCP conventions

Loads when working on the UCP packages. The code
wins over this page.

- **Every canonical UCP tool negotiates** (`internal/ucp/negotiate.go`,
  `profiles.go`): the `meta.ucp-agent.profile` is fetched (https, no
  redirects, public addresses, 256 KiB, cached ≥60s, failures backed
  off, global rate budget), validated against the embedded spec schema
  (`internal/ucp/spec/`), version-checked, and intersected with
  `BusinessCapabilities` — the same registry the profile publishes.
  Responses declare the negotiated capabilities; a missing root
  capability returns the `error_response` branch
  (`capabilities_incompatible`) of the tool's `oneOf` output schema.
  The five checkout tools share these opening steps (`negotiated` in
  `internal/mcpsrv/ucp_checkout.go`), and update, complete and cancel
  share taking the session's lock and answering through `finish`
  (`onSession`).
  The order webhook URL comes ONLY from the negotiated order
  capability's `config.webhook_url`, and `pay_way_id` needs the
  hosted-selection extension negotiated, not just the tenant gate.
- **The canonical UCP tools take the wire shapes from the OpenRPC document**
  (`create_checkout`, `get_checkout`, `update_checkout`,
  `complete_checkout`, `cancel_checkout`): `meta` (with
  `ucp-agent.profile`, plus `idempotency-key` on complete/cancel: 1 to
  255 visible ASCII characters, `checkout.ValidIdempotencyKey`, refused
  in `MetaIn.validate` before anything is read or written; the spec asks
  for 128 bits of entropy, "e.g., UUID v4, 22+ char alphanumeric", and
  sets no maximum; 255 is ACP's bound, while ACP's own key is any
  string of 1 to 255 characters, as its spec makes it opaque), `id`
  for the target session, and `checkout` for the domain object with
  snake_case members. Extensions compose FLAT onto `checkout`
  (`discounts`), never nested under capability names.
  Payment is selected by submitting an ADVERTISED instrument
  (`checkout.payment.instruments`), which the business resolves to a
  pay-way. An ONLINE method is NOT an instrument (nothing for a platform
  to acquire; the buyer pays on the PSP's page) and cannot be an Action
  either (both standard payment Action types require a
  `payment_instrument_id`) — UCP models it as `requires_escalation` +
  `continue_url`. Choosing one travels as the DECLARED extension
  `space.grooveshop.payments.hosted_selection` carrying `pay_way_id`,
  advertised and accepted only while `HostedPaymentOn()`; the gate is
  the Tenant plan flag AND the `AGENT_HOSTED_PAYMENT_ENABLED`
  extra-setting, folded by Django into `agentHostedPaymentEnabled`.
  Every tenant gate (`agentCommerceEnabled`, `productFeedsEnabled`,
  `agentHostedPaymentEnabled`) is a plain boolean Django always emits;
  a payload without one decodes as OFF, and the `*On()` accessors
  encode the subordination to agent commerce. A gated `pay_way_id` is
  REFUSED, never ignored: ignoring would complete against a stale
  selection. `create_checkout` and `update_checkout` settle every
  lookup and refusal (region, payment) before their first cart write,
  creating the cart included, so a refused call leaves the cart as the
  stored session knows it. `create_checkout` also takes `cart_id`, for
  a cart built with the cart tools. UCP models it in the cart
  capability (`dev.ucp.shopping.cart`), as a member of `checkout`; the
  profile does not advertise that capability, so the member sits beside
  `checkout`, outside the negotiated shape, and the create schema takes
  it or `checkout.line_items`. The capability's aim is
  kept, a cart has one incomplete checkout, by other means: the newest
  create holds the cart and an open checkout on it is canceled. A
  checkout is not bound to its platform yet, so the spec's "return the
  existing checkout" is not offered (`createFromCart`; "One live
  checkout holds a cart" in `CLAUDE.md`).
  These five tools and `get_order` are never
  registered on the chat server (`mcpOnly` in
  `internal/mcpsrv/server.go`) — their input requires `meta`, which a
  model cannot supply, and chat hands over a checkout link instead.
- **The tools' input schemas are the spec's** (`internal/mcpsrv/ucp_inputs.go`).
  `checkout` is the request schema the spec's `ucp-schema` resolves per
  operation from checkout composed with every advertised extension that
  adds request members (`internal/ucp/spec/requests/compose.json`: the
  discount extension), checked in beside the vendored spec by
  `scripts/ucp-request-schemas.sh`; `TestRequestSchemasAreCurrent`
  refuses files generated from another spec tree. They are open: a
  member no handler reads is dropped at decode, never refused. The
  generated files are used as they are; the business's own members are
  composed onto them where the schemas are assembled (`checkoutRequest`:
  `allOf` of the spec's schema and the own members, as an extension
  composes), each described as the business's own: `cart_id` beside
  `checkout` on create, `fulfillment` in `checkout` on create and
  update, `pay_way_id` in `checkout`.
  `TestRequestStructsDecodeWhatTheSchemasAccept` keeps each request
  struct to members its schema describes. The `ucp-schema`
  that made them (`spec/requests/generator`) bundles an instrument's
  `{"$ref": "#"}` onto the Payment object, so the schema accepts any
  instrument: `resolvePayWay` checks it.
- **A member of an extension that was not negotiated is ignored,
  silently**, except `pay_way_id` (refused, above). `discounts` is read
  only while `dev.ucp.shopping.discount` is negotiated; the profile
  advertises it while the store's promotions are on.
- **The gateway's own flat `fulfillment`** (`checkout.Fulfillment`,
  camelCase) is how a delivery is sent until the spec's fulfillment
  extension is implemented (#109 step B, after Django #641), and it
  stays in the published create and update schemas until then
  (`ownFulfillment`): inferred from the struct, so its fields, their
  descriptions (the struct's `jsonschema` tags) and which are required
  cannot drift from what is decoded, and marked in its description as
  the business's own shape the extension will replace. It is read only
  when it names its `kind`, and the schema requires the carrier and the
  whole postal address only then (`if`/`then`): the extension's own
  member has the same name and another shape (`methods`), and is
  accepted and ignored as not negotiated.
- **update_checkout replaces the checkout** ("the platform is REQUIRED to
  send the entire checkout resource"). `line_items` is required and
  replaces the cart's lines; `buyer` is what the request carries, none
  if omitted; with the discount extension negotiated, so are the codes:
  an omitted `discounts` (or `codes`) is no codes, and the cart's
  coupon is cleared (Django's removal is idempotent, so it is asked
  whatever the checkout submitted). The spec is silent on an omitted member under replacement;
  the rule is that a member the response renders is replaced, and one
  it does not render cannot be sent back, so it is kept when omitted:
  the flat `fulfillment`, and the payment selection
  (`payment.instruments`, `pay_way_id`; `payment: {}` names none).
  Without the extension, `discounts` is ignored and the codes are kept.
  A kept method is kept only while the checkout's delivery is offered
  it (`payWayForDelivery`): Django offers some methods only for some
  carriers and kinds, and an instrument is resolved for the delivery the
  checkout had then. One the delivery is not offered is replaced by the
  method that settles the same instrument for it, or dropped when none
  does, and the checkout then asks for a method.
- **An instrument resolves to the method the checkout holds** when that
  method settles it for the delivery, else to the first that does
  (`instrumentPayWay`): a store may settle one instrument with several
  methods, each for other carriers, so naming the held instrument again
  is never a change of method.
- **complete_checkout is a command, not an update** (the spec's `main`):
  it never places an order at a total the checkout did not show. The
  tag requires `payment` on complete, so a platform may first name its
  method there. By the pay way the complete's instrument (or
  `pay_way_id`) selects (`completeSession`):
  - the one the checkout holds, or none named (`payment: {}`) while it
    holds one: the order is placed;
  - another than the one it holds: the checkout as stored, `status:
    incomplete`, and a `recoverable` error `payment_method_changed`;
  - the first one, for a checkout that holds none, with no fee for this
    cart (its free threshold included): the method is taken. It is
    stored after the completion is claimed and before the flow, so a
    refused complete keeps it;
  - the first one, with a fee: `incomplete` and a `recoverable` error
    `payment_fee_added` saying the fee and the new total. The fee is the
    render's own (`checkout.ComputePricing` on the checkout as it would
    be with the method, `firstMethodFee`); a fee that cannot be read is
    `unavailable`, never "no fee", and a `pay_way_id` the store does not
    have is `not_found`, read before the fee.

  An `incomplete` answer stores nothing, claims no completion (the same
  key can be used again) and places nothing; the stored checkout keeps
  its status. The platform selects the method with update_checkout,
  which shows the new fee and total.
- **continue_url** is only where the spec requires it,
  `requires_escalation`: the PSP's page, else the order's, else the cart
  claim. Not in an open checkout (`incomplete`, `ready_for_complete`),
  though the spec SHOULDs one there: the only page is the cart's claim
  link, and a link handed out outlives the status. Once the checkout
  places an order that awaits its online payment, Django keeps the cart
  and refuses a second order from it only from an agent (django-api
  #693), so a buyer who opened that link could order the cart again on
  the web. Not in `complete_in_progress` (its order may exist) or a
  terminal status.
- **`discounts` in the response** (extension negotiated): `codes` are
  the cart's: the coupon the cart holds (applied or refused now),
  whoever attached it, then the codes the store refused when they were
  submitted, which their warnings point at. A submitted code the cart no
  longer holds is not listed: sent back, it would replace the cart's
  coupon. So a platform sending the checkout back keeps exactly them,
  and one leaving them out clears them. A placed order lists the codes
  it was submitted with. `applied` from Django's per-offer breakdown (`automatic` without
  a `code`; none for a placed order, whose discounts are one sum); each
  refused code a `warning` message with its rejection code and `path`
  `$.discounts.codes[i]` (`checkout.Rejections`).
- The service `schema` (OpenRPC) URL may appear ONLY while every method it
  defines for an ADVERTISED capability exists — the five checkout tools
  plus `get_order`. Declaring it asserts a machine-checkable contract.
  Its cart/catalog methods belong to capabilities the profile does not
  advertise, so a negotiating platform never calls them.
- `get_order` sources the required `checkout_id` from the gateway's own
  order link (`ag:{schema}:orderlink:{uuid}`: checkout id, storefront
  domain, webhook target; retained a year — it must outlive the session,
  which expires in 24h). An order placed on the web has no checkout to
  name and is refused with that explanation, never a fabricated id.
  `order.fulfillment` stays `{}`: an expectation needs a destination the
  order detail deliberately does not decode (PII), and an event needs a
  shipment timestamp upstream does not expose.
- Order webhooks route through that link, never the session, and carry
  the full UCP order entity rendered at enqueue. They are signed per RFC
  9421 with the tenant's ES256 key (`internal/httpsig`; UCP's baseline
  algorithm), with Standard Webhooks `Webhook-Id`/`Webhook-Timestamp`
  and `UCP-Agent` naming the tenant profile. The signature covers
  `webhook-id` and `webhook-timestamp` beside UCP's REST components
  (`signedComponents`), as UCP after 2026-08-25 requires of an order
  webhook. The queue is a Redis stream
  consumer group (`ag:webhooks:orders`, one consumer per pod name): a
  delivery is acknowledged only on a terminal outcome, a consumer's own
  pending entries replay on restart, and a dead consumer's are claimed
  after the visibility timeout. Delivery dials only public addresses and
  never follows redirects.

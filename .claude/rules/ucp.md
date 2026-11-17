---
paths:
  - "internal/ucp/**"
  - "internal/mcpsrv/ucp_*.go"
  - "internal/mcpsrv/order.go"
  - "internal/mcpsrv/signature.go"
  - "internal/httpsig/**"
  - "cmd/ucp-sign/**"
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
  (`onSession`). Their refusals are `mcptool.ToolError`s whose codes
  are listed in `.claude/rules/mcp-tools.md`; a checkout that is
  canceled or placed its order is `checkout_closed`, which the spec's
  `error_code.json` does not define.
  The order webhook URL comes ONLY from the negotiated order
  capability's `config.webhook_url`, and `pay_way_id` needs the
  hosted-selection extension negotiated, not just the tenant gate.
  On a signed request, `meta.ucp-agent.profile` must name the platform
  the signature proved (Identity Binding: `checkSigner` in `negotiate`,
  and `VerifyPlatformSignature` before the handler), else
  `signature_invalid`.
- **A platform's RFC 9421 signature** (`signatures.md`, MCP Transport)
  is verified on every UCP request to `/mcp` that carries one (it names
  its platform in `UCP-Agent`, or calls `get_order`; another request's
  signature is not UCP's to judge, and it is served unsigned)
  (`mcpsrv.VerifyPlatformSignature`, `ucp.ProfileResolver.VerifyRequest`,
  `httpsig.Verify`; Signatures, below, names the rules), with the keys in the
  top-level `keys[]` of the profile its `UCP-Agent` header names
  (`PlatformProfile.Keys`; a key marked `use: enc`, or with `key_ops`
  that leave out `verify`, is skipped). The signer is that profile's URL,
  normalized (`NormalizeProfileURL`: scheme and host lowercased, IDNA
  A-label, default port dropped, empty path as `/`, no fragment).
  Policy, a fixed one with no switch:
  - `get_order` takes only a signed request (`signed` in
    `internal/mcpsrv/server.go`): unsigned, it is 401 `signature_missing`;
  - a checkout a signed `create_checkout` made belongs to that signer
    (`Session.Platform`): every later call on it, and the create that
    would take its cart, must come from the same signer, and any other
    caller gets the missing-session answer (`Session.Serves`);
  - an unsigned `create_checkout` stays open, as the spec makes signing
    a SHOULD for checkout: its checkout belongs to no platform, and any
    caller reaches it.

  Requiring signatures on the other five tools is a release of its own,
  never a setting. `created` and `expires` are held to
  `UCP_SIGNATURE_MAX_AGE` and `UCP_SIGNATURE_CLOCK_SKEW` only when the
  signature carries them; UCP's own replay protection is the idempotency
  key. `cmd/ucp-sign` signs requests to a running gateway as a platform
  (`ucp.SignRequest`), for the runbook and the staging check.
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
  the spec's snake_case members (and `cart_id`, `pay_way_id`), except
  the gateway's own flat `fulfillment`, camelCase until #109 step B
  (below). Extensions compose FLAT onto `checkout`
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
  create holds the cart and an open checkout on it is canceled, unless
  another platform signed its creation. A checkout created unsigned is
  bound to no platform, so the spec's "return the existing checkout" is
  not offered (`createFromCart`; "One live checkout holds a cart" in
  `.claude/rules/checkout.md`).
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
  `requires_escalation`: the PSP's page while its link can be paid,
  else the order's (never the cart's). Not in an open checkout (`incomplete`, `ready_for_complete`),
  though the spec SHOULDs one there: the only page is the cart's claim
  link, and a link handed out outlives the status. Once the checkout
  places an order that awaits its online payment, Django keeps the cart
  and refuses a second order from it only from an agent (django-api
  #693), so a buyer who opened that link could order the cart again on
  the web. Not in `complete_in_progress` (its order may exist) or a
  terminal status.
- **A checkout awaiting its payment** (`requires_escalation`) is
  rendered from its order (`ucp.awaitingPayment`), with one `error`
  message whose severity is a `requires_*` one, as the spec requires of
  that status. On the PSP's page, which can be paid until the order's
  `paymentHoldExpiresAt` (named in it): `payment_required`, and
  `payment_failed` for a failed attempt, which Django lets the buyer
  retry there (`requires_buyer_input`). On the order's page
  (`requires_buyer_review`): `payment_processing` (the provider is
  processing a payment; never pay again), `payment_in_review` (nothing
  will decide the order: a charge that did not match its total waits
  for the store, or the sweep is off, which Django does not tell apart,
  so neither "pay" nor "nothing is charged" is said) and
  `payment_link_expired` (its time to pay ran out: the store cancels
  it, nothing is charged). And `payment_link_unavailable`
  (`recoverable`, the order's page: the first link could not be
  created, and complete_checkout tries again).
  Its `expires_at` is when Django will have decided the order
  (`.claude/rules/checkout.md`). A canceled order makes the checkout
  `canceled` with an `info` message `order_canceled` (nothing was
  charged, or a payment after the cancel is refunded); a charged one
  `completed`. No link is minted after the first: an expired one means
  the order is canceled by `expires_at` and nothing is charged, and the
  cart can be checked out again. The tag's `complete_in_progress` is
  rendered for a checkout whose order is being placed; its Actions are
  not used: the one step a buyer takes is the PSP's page, which
  `continue_url` carries.
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
- `get_order` serves an order to the platform that placed it, as the
  order capability's Authorization section describes ("Platform
  credentials: orders originated by the platform"): the order link
  (`ag:{schema}:orderlink:{uuid}`: checkout id, storefront domain,
  webhook target, and the platform that signed the checkout's creation;
  retained a year, as it must outlive the session: an open one lives
  30 minutes after its last save, one whose order exists or may 24
  hours, `.claude/rules/checkout.md`) is read first, and an order whose link names another platform,
  or none, is the spec's `not_found` error response
  (`ucp.OrderNotFound`), the same as an id that names no order and
  without asking the store. The order is then read with the gateway's
  own credential (`django.Client.PlacedOrder`), for as long as its link
  is kept. The required `checkout_id` comes from the link, never a
  fabricated id.
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
- **One webhook target holds at most its share of a pod's workers**
  (`perTargetDeliveries` of `deliveryWorkers`, `targetSlots`). The
  target is the registrable domain of the URL's host, its IDNA A-label
  (`deliveryTarget`: eTLD+1 by the public suffix list, its private
  section included), or the address as the rate limiter counts one
  (`httpmw.AddrBucket`): a name under one's own domain is free, so a cap
  per host or origin would give one owner as many shares as names. A
  delivery read while its target is at the cap waits in process in the
  target's queue, first in first out, and the worker that frees one of
  the target's slots takes it next, so a burst to a busy platform goes
  out as fast as it answers. A delivery beyond the queues' bounds
  (`maxQueuedPerTarget`, `maxQueued`), or one not started within
  `queueMaxWait` of its read (waiting for its target's slot or for a
  free worker, `handOut`), is left pending, never acknowledged, for
  reclaim after the visibility timeout. The read time is stamped before
  the Redis call, and a delivery is cut at `deliveryBudget`, so what a
  pod holds ends within `queueMaxWait` + `deliveryBudget` + `ackTimeout`
  of its read, under the visibility timeout: no entry is taken over
  while its pod still works on it. A reclaim sweep goes on from its last cursor until it has
  scanned every pending entry. So an endpoint that does not answer
  delays its own deliveries, never another platform's.
- **Order webhooks carry no ordering guarantee.** Up to two deliveries
  to one target run at once, a failed attempt is retried after a
  backoff, and reclaim takes entries over later, so two events of one
  order can arrive in either order; each carries the full order entity.
- **A key that cannot be read is never a terminal outcome**: a delivery
  is acknowledged as failed only when every attempt reached the
  endpoint; one whose store's key could not be read stays pending.
- **The signing key is sealed at rest** (`internal/ucp/keys.go`):
  Redis, its AOF files and their backups sit outside the process, and
  the scalar signs a store's order webhooks.
  `ag:{schema}:ucp:signing_key:es256:aes256gcm` holds the private
  scalar sealed with AES-256-GCM (`cipher.NewGCMWithRandomNonce`) under
  `SIGNING_KEY_ENCRYPTION_KEY`, with the purpose and the schema as
  associated data (`sealContext`), so a record copied to another store
  does not open. A record that does not open is an error, never a key
  minted over it: a new key would rotate the store's published key
  under the platforms that verify against it. At boot a pod checks its
  key-encryption key against the sealed keys Redis holds (`AwaitKEK`):
  it retries until Redis answers, carrying its SCAN cursor across
  attempts so a scan longer than one attempt completes, is not ready
  until the check passed (`Checked`, in `/readyz`), and exits only when
  Redis holds keys and the key opens none of them. The records carry no
  TTL: the gateway's Redis is its own instance with `noeviction`, so
  nothing evicts them. A pod holds a key in memory from its first use.
  The move from the
  unsealed record (`ag:{schema}:ucp:signing_key:es256`,
  `keys_unsealed.go`) keeps every key, kid and published JWK included:
  a store's unsealed key is sealed the first time a pod needs it; a key
  minted for a store with none leaves a sentinel no earlier release can
  decode in the unsealed record, expiring after 7 days, so an older pod
  never mints a second key; `/gateway seal-signing-keys` seals the rest and deletes the
  unsealed records and sentinels once no older pod is left. That code
  goes in the release after every environment has run the command
  (`docs/agent-protocols-runbook.md`, section 12).

## Signatures

- `internal/httpsig` — RFC 9421 HTTP message signatures with RFC 9530
  `Content-Digest`: `Sign` for the order webhooks and `Verify` for a
  platform's requests, structured fields read and written with
  `github.com/dunglas/httpsfv`. `Verify` applies UCP's Identity
  Resolution Algorithm: each signature on its own, one that verifies is
  enough; a tag other than `web-bot-auth` is skipped; an `alg`
  parameter is refused (the algorithm is the key's: ES256, ES384,
  Ed25519, ECDSA as fixed-width r||s); a `web-bot-auth` signature's
  keyid must be its key's RFC 7638 thumbprint; it must cover, each
  whole (no parameter), `@method`, `@authority`, `@path`, `@query` when
  there is one, the body (`content-digest`, `content-type`) and each of
  `ucp-agent` and `idempotency-key` the request carries, and
  `signature-agent` whole or by the member keyed by the signature's
  label; a covered field takes only the `key` parameter (RFC 9421
  section 2.1.2). A received request's `@authority` is its `Host`, either
  scheme's default port dropped (TLS ends at Traefik). The refusals
  carry UCP's codes (`signature_invalid`, `key_not_found`,
  `digest_mismatch`, `algorithm_unsupported`).
- **A platform's signature on `/mcp`** (`mcpsrv.VerifyPlatformSignature`,
  after the identity middleware). A UCP request that carries
  `Signature-Input` or `Signature`, one with a `UCP-Agent` header or one
  that calls a tool marked `signed`, is verified, whatever it calls,
  against the keys its `UCP-Agent` profile publishes
  (`ucp.ProfileResolver.VerifyRequest`: the profile is fetched as
  negotiation fetches it, and fetched again once a TTL floor per origin
  for a keyid it does not publish, `Refresh`, which replaces the cached
  profile only with one it fetched; a key that does not decode is
  skipped, never the profile refused). A signature on any other request
  (an agent that signs all its traffic, as Web Bot Auth has it) is not
  verified, and the request is served as unsigned. At most
  `httpsig.maxSignatures` signatures are tried. One that does not verify
  is answered before the MCP handler with UCP's HTTP status and
  JSON-RPC error: 401 and -32000 (`signature_invalid`,
  `key_not_found`), 400 and -32600 (`digest_mismatch`,
  `algorithm_unsupported`), or the discovery failure (-32001 with
  400/424/422, or 503 when the budget is spent). A signed request whose
  tool call names another platform in `meta.ucp-agent.profile` is
  `signature_invalid` (Identity Binding; `negotiate` checks it again on
  the meta the tool decoded). The verified platform, its profile URL
  normalized (`ucp.NormalizeProfileURL`), rides the request context. A
  tool marked `signed` (`get_order`) refuses an unsigned call with 401
  and `signature_missing`, with no `WWW-Authenticate`: no OAuth link
  answers it. A caller that left during the check gets a 503 and no log
  line. The body is read once for this and the account challenge
  (`readPost`), as the SDK reads it.

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
  off, a global fetch budget with each caller's share of it, see
  below), validated against the embedded spec schema
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
- **The discovery budget** (`internal/ucp/profiles.go`, `join` and
  `spend`): a fetch costs one token from the pod's shared budget
  (`fetchRate`, `fetchBurst`) and one from its caller's share
  (`callerFetchesPerMin`, `callerFetchBurst`, whose comment says why),
  or neither, charged before anything is fetched. Resolve shares one
  fetch per URL: the caller that starts it pays for it, each other
  caller that waits on it pays its own share once (so a fetch under way
  never answers for a caller past its share), and a caller's concurrent
  calls for one profile cost it one token. A refresh pays as a fetch. A spent one
  is `ErrDiscoveryBusy`, -32000 with `retry_after` that budget's delay
  (`DiscoveryRetryAfter`). A cached profile is served whatever is left.
  The caller is `httpmw.ClientFrom`: `ClientBucket` alone (an IPv4
  address, an IPv6 /64), not the per-pod limiter's (store host, caller)
  key, so platforms behind one egress address or /64 share one share
  across every store. That is enough at today's volume (production,
  14 days to 2026-11-21: 276 POSTs to /mcp, at most 12 a minute from
  every caller together) and keeps a caller from multiplying its share
  by the number of stores. A context that names no caller fetches
  nothing (`errNoCaller`): every route that resolves a profile sits
  behind the limiter's middleware.
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
  the spec's snake_case members (and `cart_id`, `pay_way_id`).
  Extensions compose FLAT onto `checkout` (`discounts`, `fulfillment`),
  never nested under capability names.
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
  discount and fulfillment extensions), checked in beside the vendored
  spec by
  `scripts/ucp-request-schemas.sh`; `TestRequestSchemasAreCurrent`
  refuses files generated from another spec tree. They are open: a
  member no handler reads is dropped at decode, never refused. The
  generated files are used as they are; the business's own members are
  composed onto them where the schemas are assembled (`checkoutRequest`:
  `allOf` of the spec's schema and the own members, as an extension
  composes), each described as the business's own: `cart_id` beside
  `checkout` on create, `pay_way_id` in `checkout`. Each extension
  composes its own copy of the checkout, so the create schema's
  `required: [line_items]` is dropped from every copy (`dropRequired`).
  `TestRequestStructsDecodeWhatTheSchemasAccept` keeps each request
  struct to members its schema describes. The `ucp-schema`
  that made them (`spec/requests/generator`) bundles an instrument's
  `{"$ref": "#"}` onto the Payment object, so the schema accepts any
  instrument: `resolvePayWay` checks it.
- **A member of an extension that was not negotiated is ignored,
  silently**, except `pay_way_id` (refused, above). `discounts` is read
  only while `dev.ucp.shopping.discount` is negotiated; the profile
  advertises it while the store's promotions are on. `fulfillment` is
  read only while `dev.ucp.shopping.fulfillment` is negotiated, which the
  profile always advertises: without it a platform can give no address,
  and the checkout's `missing` error at `$.fulfillment` says so.
- **The delivery is the spec's fulfillment extension** (#109 steps B
  and C; request `mcpsrv.UCPCheckoutIn.delivery`, response
  `ucp.renderFulfillment`, in `internal/ucp/fulfillment.go`). The store
  fulfils a cart one way, so a checkout has two methods at most: the
  `shipping` method (`id` `shipping`, `methods[0]`), always listed, and
  the `pickup` method (`id` `pickup`, `methods[1]`), listed where the
  store delivers the cart to pickup points in the country (a
  `pickup_point` option the cart fits) or when the checkout chose it.
  The chosen method holds every line in `line_item_ids`, the other
  none; with nothing chosen, shipping holds them. A request chooses with
  its only method, or the one of its two whose `line_item_ids` name the
  lines; with neither naming them, it chooses none.
  - **shipping:** one destination (`shipping_address`), the platform's
    postal address. `street_address` is the one street line with the
    house number after the street, sent to Django as `street`, which
    splits it; `extended_address` is the order's `addressLine2`;
    `address_region` names the region, one the store delivers to at home
    (`checkout.ResolveRegion`); its
    contact members are not read (the order's contact is `buyer`). A
    request that names `selected_destination_id` `shipping_address` and
    writes no destination keeps the stored address. One group
    (`parcel`, holding the method's lines: none while pickup is chosen):
    its `options` are the carriers that deliver the cart home
    as Django quotes it (`Pricing.ShippingOptions`, home delivery the
    cart fits), each `id` its provider code with its price as a `total`,
    listed once the country is known; `selected_option_id` chooses one,
    null until then.
  - **pickup:** the destinations are business locations
    (`business_location`, a name and an address) the platform selects by
    `selected_destination_id`, never writes. A location's id is opaque
    (`ucp.PickupLocation.DestinationID`: carrier, the point's own
    country, the carrier's ids, each query-escaped, joined by colons; an
    ACS station's branch may be blank, the catalogue's row without one),
    read back by `ucp.ParsePickupLocation`, which refuses a country that
    is not two upper-case letters, the only form this business gives; one this checkout never listed is taken
    (the spec's "any stable, Business-scoped ID"), for example a
    `destinationId` from `find_pickup_points`. The destinations are the
    ACS points nearest the checkout's locality (`checkout.context`'s
    `address_country` and `postal_code`, `Session.Locality`, kept when
    omitted since the response does not carry it, a country that is not
    two letters ignored as the hint the spec makes it; at most five, one
    `AcsNearestStations` call per render in the country the options are
    priced for, `checkout.QuoteRequest`'s, none without one; a failed
    one a `warning` `pickup_points_unlisted`), and the selected point, by the name and
    address the store's catalogue gave when it was chosen
    (`handlers.applyPoint`: `AcsStationByID`, `BoxNowLockerByID`, one
    call each to the catalogue lists with Django's exact filters,
    `GET /shipping/acs/stations?externalId=&branch=&countryCode=` and
    `GET /shipping/boxnow/lockers?externalId=&countryCode=`, from
    django-api #767; an ACS area code alone names every Smartpoint of
    its area, so the branch is always sent; one
    the catalogue does not hold is taken and named by its carrier and
    id, and Django decides at the order). BOX NOW lockers are found only
    from a street, so only `find_pickup_points` lists them. A location
    is always named: one the catalogue holds without a name, or does not
    hold, by its carrier and id. No group until a point is selected,
    then one (`pickup_parcel`) with that point's network as its one
    option, selected. A pickup order carries no
    address and no region: Django delivers it to the point's address
    (django-api #767).
  - An update may name a method by its `id` (`shipping`, `pickup`)
    instead of its `type`, which the spec makes optional there
    (`mcpsrv.methodType`).
  - A placed order lists no options. Refused, and nothing applied (the
    buyer included): another method type, a method `id` the business
    never gave or that contradicts its `type`, a method named by
    neither, two of one type, both naming the lines, a second
    destination or group, a destination id this business never gives or
    that is not the destination written.
  - What a checkout still needs is an `error` message, `recoverable`,
    code `missing`, at its path (`ucp.missing`: `$.buyer`,
    `…methods[0].destinations`, `…destinations[0].address_region`,
    `…groups[0].selected_option_id` once the group is rendered, else
    `$.fulfillment.methods[0]`, `…methods[1].selected_destination_id`,
    `$.payment`): a path names only a member the checkout renders. A
    refused delivery is the quote's code at the shipping group's option,
    or for a pickup at the pickup method's `selected_destination_id`
    (its remedy another point or shipping).
  - **A destination Django refuses** at order creation
    (`django.DestinationRefusedError`: any code on `street` but a street
    not sent, `street_number_missing` and `street_number_is_postcode`
    worded by the gateway; `pickup_point_unknown`,
    `pickup_point_in_another_country`, `pickup_point_required` on the
    carrier's location field; `region_not_served` on `regionId`, a region
    the store's shipping zones no longer reach; read from
    `fields` whatever the body's own code) is stored on the session
    (`Session.DestinationRefused`) until the destination changes
    (`Session.Deliver`) or, for a region, until one is read that the
    store serves (`checkout.ResolveRegion`): the checkout reads
    `incomplete` with that code
    at the street (`…destinations[0].street_address`), the region
    (`…destinations[0].address_region`) or the selection
    (`…methods[1].selected_destination_id`), and the complete that met
    it is answered with that checkout, nothing placed.
- **update_checkout replaces the checkout** ("the platform is REQUIRED to
  send the entire checkout resource"). `line_items` is required and
  replaces the cart's lines; `buyer` is what the request carries, none
  if omitted; with the discount extension negotiated, so are the codes:
  an omitted `discounts` (or `codes`) is no codes, and the cart's
  coupon is cleared (Django's removal is idempotent, so it is asked
  whatever the checkout submitted). The spec is silent on an omitted member under replacement;
  the rule is that a member the response renders is replaced, and one
  it does not render cannot be sent back, so it is kept when omitted:
  the payment selection (`payment.instruments`, `pay_way_id`;
  `payment: {}` names none). With the fulfillment extension negotiated,
  an omitted `fulfillment` is no delivery. Without an extension, its
  member is ignored and what the checkout holds is kept.
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
    refused complete keeps it. The order is placed from the cart the
    checkout showed: the digest holds no pay way (django-api #770), so
    the platform's last read already carries the one the order is
    checked against, and a cart changed since, at the same total or not,
    is refused `cart_changed`. A delivery the store refuses now is named
    first (`firstMethodFee`, as `Flow.admit` names it);
  - the first one, with a fee: `incomplete` and a `recoverable` error
    `payment_fee_added` saying the fee and the new total. The fee is the
    render's own (`checkout.ComputePricing` on the checkout as it would
    be with the method, `firstMethodFee`); a fee that cannot be read is
    `unavailable`, never "no fee", and a `pay_way_id` the store does not
    have is `not_found`, read before the fee.

  An `incomplete` answer stores nothing, claims no completion (the same
  key can be used again) and places nothing; the stored checkout keeps
  its status. The platform selects the method with update_checkout,
  which shows the new fee and total. A first method the store does not
  offer for the delivery is `unsupported`.

  The order is placed at the total the checkout showed, from the cart
  it showed (`Session.Shown`, `.claude/rules/checkout.md`). One Django
  refuses because the total changed since is the tool error
  `total_changed`, its completion claim released, and shows no total:
  the platform reads the checkout, which shows and stores the total now.
  One refused because the cart changed since (`cart_changed`) is
  answered the same way. A retry under the same key runs again and sends
  what was stored before, so it places nothing at a total or from a cart
  no answer showed, and a complete after the read places the order at
  what the read showed.
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
  that status, checked in this order:
  - `payment_processing` (the provider is processing a payment) and
    `payment_in_review` (`paymentAwaitsReview`: a charge that did not
    match the total waits for the store): the order's page,
    `requires_buyer_review`, never pay again, and never "owes nothing";
  - `payment_link_expired`: its hold (`paymentHoldExpiresAt`) has
    passed, the order's page, `requires_buyer_review`: the shopper owes
    nothing, and the store cancels it by `expires_at` when its sweep will
    decide it (a store whose sweep is off is promised no cancel, here or
    in `payment_failed`);
  - `payment_with_store`: Django holds no payment open for the order (no
    hold), the order's page, `requires_buyer_review`, and no link
    is minted for it;
  - `payment_link_unavailable`: no link yet, the order's page,
    `recoverable`: complete_checkout tries again;
  - `payment_failed` (a failed attempt, which Django lets the buyer
    retry) and `payment_required`: the PSP's page, `requires_buyer_input`,
    naming when the link stops being payable. A store whose sweep is off
    (`paymentDecidedBy` null, nothing charged) is paid here too.

  Its `expires_at` is when Django will have decided the order
  (`.claude/rules/checkout.md`). A canceled order makes the checkout
  `canceled` with an `info` message `order_canceled` (the shopper owes
  nothing: Django settles any unconfirmed payment as canceled with its
  order, so it is never "nothing was charged"; a payment after the
  cancel is refunded); a charged one `completed`. No link is minted
  after the first, nor the first once the order's hold has passed or
  where the checkout tells the buyer not to pay
  (`Flow.ResumePayment` reads the order; `ErrPaymentAwaitsStore`).
  The tag's `complete_in_progress` is rendered for a checkout whose
  order is being placed; its Actions are not used: the one step a buyer
  takes is the PSP's page, which `continue_url` carries.
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
  the full UCP order entity rendered when the event is sent. They are signed per RFC
  9421 with the tenant's ES256 key (`internal/httpsig`; UCP's baseline
  algorithm), with Standard Webhooks `Webhook-Id`/`Webhook-Timestamp`
  and `UCP-Agent` naming the tenant profile. The signature covers
  `webhook-id` and `webhook-timestamp` beside UCP's REST components
  (`signedComponents`), as UCP after 2026-08-25 requires of an order
  webhook. Delivery dials only public addresses and never follows
  redirects, and signs nothing before the boot check of the
  key-encryption key passed (`Keys.Checked`).
- **Django's outbox keeps each event; the gateway keeps none**
  (django-api `agent/outbox.py`, `internal/server/internalapi.go`).
  The outbox calls `POST /internal/events/order-status` with its row's
  `eventId` and `occurredAt`: the `Webhook-Id` and the
  `Webhook-Timestamp`, the same on every attempt, as UCP requires of a
  retry (the signature's `created` is the attempt's: a platform that
  judges a delivery's freshness reads that, and one that refuses an old
  `Webhook-Timestamp` departs from UCP, its 4xx failing the event for
  good). Within the call
  the gateway updates the checkout (`Flow.ApplyOrderEvent`), renders
  the order and posts it once (`ucp.Webhooks.Send`), all within
  `orderEventBudget` (25 s, under the outbox's 30 s read timeout), and
  answers the outcome:
  - 204: delivered, or nothing to deliver (the checkout named no
    webhook);
  - 503: try again later. Redis, Django or the store's key failed, the
    store does not resolve (suspended: the outbox runs only while the
    store exists), the post got no answer, or the platform answered
    408, 429 or 5xx (its `Retry-After` is passed on). A checkout another
    request stored meanwhile (`ErrSessionChanged`) asks for the
    outbox's shortest wait. A key that cannot be read is never final;
  - 422: failed for good. The platform answered anything else (a
    redirect included), the target is not a public address, the store's
    domain is another store's, or Django no longer serves the order;
  - 400: a body without its store, order, event id (a UUID) or time
    (RFC 3339).

  An order without a link is answered 503 with `Retry-After` until
  `checkout.CompletionBudget` has passed since the event: Django sends
  an order's first event as it commits, before the completion that
  placed it writes the link (`Flow.place`). After that, 204 and a warn
  line: the link is lost (or, for an order whose creation Django never
  answered, was never written). `gateway_order_webhooks_total{outcome}`
  counts `delivered` and the gateway's own `failed` (its 422s); an event
  the outbox gives up on after retries is counted by Django, not here.
  A failed post's error names the target's host, never its URL (a
  platform may carry a token in the query). Retries, their backoff and their
  order are the outbox's: an order's events go out in the order they
  happened, and each store is swept on its own, one event at a time.
  So a platform that does not answer holds up only its own store's
  retries, each by up to `postTimeout`; nothing caps one platform's
  share within a store.
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
  (`docs/agent-protocols-runbook.md`, "Sealing the stores' signing keys").

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

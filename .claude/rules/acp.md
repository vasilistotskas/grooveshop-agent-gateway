---
paths:
  - "internal/acp/**"
  - "internal/server/**"
---

# ACP (`/acp/*`)

Loads when working on the ACP handler, or the routes that answer before it. The code wins over this page.

- **ACP errors are the spec's** (`internal/acp/handler.go`): every
  error answered on `/acp/*` is checked against the vendored `Error`
  schema, which requires `type`, `code` and `message`.
  - What answers before the handler is the spec's too, with its status
    and message unchanged: the tenant middleware's 404 and 503, the
    checkout gate's 404, the rate limiter's 429 (with `Retry-After`),
    `Recover`'s 500 and the mux's own 404 and 405 (with `Allow`), each
    mapped by status to the spec's `type` and a code
    (`acp.WriteRouteError`). The server mounts `acp.WriteRouteError` on every path
    under `acp.PathPrefix` (`surfaceErrors`, outermost), in the request
    context (`httpmw.WithErrorWriter`); `httpmw.WriteError`, `Recover`
    and `server.unrouted` write through it, so `internal/tenant` and
    `internal/httpmw` never import `internal/acp`. Other paths keep
    `{"error": msg}` and the mux's text. `/mcp` mounts none: the MCP
    SDK answers its own HTTP-level errors in plain text.
  - A store whose checkout is off answers a checkout route exactly as
    an unknown `/acp/` path does: 404 `not_found`, "not found".
  - A request whose `API-Version` is not `acp.Version` gets 400
    `missing_api_version` or `unsupported_api_version` with
    `supported_versions`.
  - An `Idempotency-Key` longer than the spec's 255 characters is 400
    `invalid` at `Idempotency-Key`, before any Redis key is built from it
    (`errKeyForm`). Any string within it is a key: the spec makes it
    opaque.
  - 409 is only `idempotency_in_flight`, with `Retry-After`: a key or
    the session lock in use, a session another request stored while
    this one ran (`checkout.ErrSessionChanged`), or an order being
    placed until `Flow.CompletionDeadline`. Past it a complete resumes
    the completion (`Flow.Resumable`, `Flow.Resume`): the order is sent
    again with the session's id, and Django answers the order the
    session placed, if it placed one. Update and cancel are answered
    502 `order_outcome_unknown` with `Retry-After`, as is a complete
    whose order is still unanswered: it says to complete again. A
    session stored unconfirmed (`Session.Unconfirmed`) is past it from
    the first call: its completion stored that as it ended. A session
    in progress whose order is known (`Flow.PlacedOrder`) is answered
    500 `order_placed`, naming the order (`writePlacedUnsaved`), until
    the deadline, and resumed by a complete after it.
  - ACP completes only with a collect-later pay way: the handler
    chooses one (`checkout.OfflinePayWay`), and `Flow.admit` refuses an
    online pay way for a session whose platform does not choose it
    (`payWaySelectionRequired`) on the store's answer when the order is
    placed, as `ErrPaymentMethodUnsupported`. So no ACP session awaits
    a payment.
  - A session that is not open refuses update and cancel
    (`refuseChange`): 405 and an `Allow` header once it is completed or
    canceled, the in-flight answer while its order is being placed. A
    canceled one refuses complete with 405. A complete of a completed
    session answers 200 with the session.
  - Every POST claims its key (`claimKey`) before it reads a session or
    takes its lock, complete included: a complete without a key is 400
    `idempotency_key_required` whatever the session is, and a replay
    is answered without the lock.
  - Each key keeps the response that was sent, status and body
    (`idemRecord`), and a replay with an equivalent body answers those
    bytes with `Idempotent-Replayed: true` for the record's 24 hours,
    whatever the session became since, or whether it lives. The answer
    is written into a `heldResponse` and sent only once the key is
    settled by it (`settle`). Another body under the key is 422
    `idempotency_conflict`.
  - An update or a cancel writes its own mark into the session with
    the save that applies it (`Session.AppliedBy`, `requestMark`: the
    endpoint, the key and the body, hashed). A retry that runs afresh
    (the first attempt was saved and could not render, so its 5xx
    freed the key; or its pod died before settling) finds the mark and
    is answered with the session (`alreadyApplied`), changing nothing:
    a cancel run again would be a 405, kept against the key that
    canceled. `finish` reads the mark too: a save that was stored
    while Redis lost its reply leaves the request holding the older
    revision, and the stored session, which holds its mark, is its
    answer. One slot: after another key's update, an earlier
    update's retry applies again.
  - Equivalence is the body's JSON (§6.2, `fingerprint`): decoded and
    encoded again, so member order and `1.0` against `1` do not count,
    `null` against an absent member does, array order does, and an
    unknown member counts (§6.7). Cancel's body is optional; an absent
    one has a fingerprint of its own.
  - What a key keeps (`keptAgainstKey`): every answer that is final
    for its request, refusals included (400, 404, 405 with its
    `Allow`), replayed for 24 hours however the conditions that caused
    it change (§6.3). A platform that wants a new attempt sends a new key,
    and a kept refusal's message ends by saying so (`noteKept`,
    `msgRefusalKept`), since its own words may name a condition that
    changes (stock, an unpaid order, missing data).
    The key is freed, and the same key runs afresh, only for an answer
    that asks for the same request again: a 5xx (§6.5), a 409
    `idempotency_in_flight` (§6.4: the key or the lock in use, a
    session another request changed) and a 429.
  - One 5xx answer is kept, departing from §6.5: 500 `order_placed`
    for an order whose session was not saved. The order exists and the
    stored session may not say so, so a retry run afresh could be told
    otherwise; and the spec has no cacheable status for "an order
    exists; do not place another" (a 4xx says the client erred, its 409
    asks for a retry, a complete's 200 must carry the order). 502
    `order_outcome_unknown` is not kept: the same key sent again
    resumes the completion, which never orders twice.
  - Keys are `ag:{schema}:acp:idem:create.v3:{key}` and
    `…:{update|complete|cancel}.v3:{session}:{key}` (`idemScope`,
    versioned by `idemVersion` with the record and the fingerprint: a
    record an older release wrote is a miss, and its key runs as after
    expiry). So the buyer data a request carried and its answer
    renders (name, email, phone, address) is kept in Redis for 24
    hours unless the record is lost (a flush or a restore), for all
    four operations, while an open
    session lives 30 minutes: the spec requires a key's response to be
    kept at least 24 hours (§6.6). Django's own idempotency cache keeps
    order responses as long, and a placed session the same data a day.
    Nothing else reads it.
  - A create or an update settles every lookup and refusal it can make
    without the cart (item ids that are not numbers, a quantity that is
    not a count, the region, the pay way) before its first cart write,
    creating the cart included, as UCP's `create_checkout` and
    `update_checkout` do with their region and payment lookups (their
    line sync and discount codes come after the first cart write): a
    create refused there leaves no cart in Django, an update the cart
    as the stored session knows it. The lines
    of one product are one cart line of their sum
    (`checkout.MergeLines`, which refuses a quantity below 1 or a sum
    past UCP's maximum as `checkout.ErrInvalidQuantity`: 400 `invalid`
    at `$.line_items` on ACP). A line sync that fails partway still
    leaves what it wrote.
  - A create is rendered before it is saved: a create that failed (a
    5xx) leaves no session and no record, and one refused (a 4xx)
    leaves no session and its refusal as the key's answer. A
    create that ran past its key's claim, with another create holding
    the key by then, discards its session and answers 409: one key
    answers one session. A create whose record could not be written
    (`storeIdem` returned an error that is not `errClaimLapsed`) does
    not know whether the key holds its response: it answers 503 and
    hands out no session, and does not discard it, because the record
    may name it. A retry of the key replays the session if it was
    recorded and creates anew if not. A 201 without its record would
    let that retry create a second session.
  - Every session carries `expires_at` (`Session.ExpiresAt`, the
    rule in `.claude/rules/checkout.md`): its own end, since no ACP
    session awaits a payment. The status `expired` is never sent: a
    session past its `expires_at` is no longer stored, and is the
    404 `msgSessionGone` that says an order may exist.
  - A complete Django refuses because the session's total changed
    since it was shown (`django.ErrTotalChanged`) is 400
    `total_changed`: nothing placed, the session ready again, and its
    next read shows, and stores, the total a complete with a new key
    places the order at. One refused because the session's cart changed
    since it was shown, at the same total or not
    (`django.ErrCartChanged`), is 400 `cart_changed`, the same way.
    Fulfillment options are the quote's
    (`Pricing.ShippingOptions`, home delivery the cart fits), and a
    delivery or pay way the store refuses is an `invalid` error at the
    member to choose again, with the status `not_ready_for_payment`.
  - The address's `line_one` is the one street line, sent to Django as
    `street` with no street number (Django splits it, and refuses a
    line it cannot split), and `line_two` the order's `addressLine2`. A
    complete Django refuses for the street line
    (`django.StreetRefusedError`) is 400 `invalid` with `param`
    `$.fulfillment_details.address.line_one`, nothing placed; the session
    then reads `not_ready_for_payment` with an `invalid` message there
    until an update changes the address (`Session.StreetRefused`).
  - `completeError` answers every `checkout` and `django` sentinel
    deliberately (`TestCompleteErrorMapsEverySentinel` walks them): a
    refusal is a 4xx, and an order that exists or may is never
    answered as a failure.

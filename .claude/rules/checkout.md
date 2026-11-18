---
paths:
  - "internal/checkout/**"
  - "internal/acp/**"
  - "internal/mcpsrv/ucp_*.go"
  - "internal/mcpsrv/order.go"
  - "internal/server/internalapi.go"
  - "cmd/gateway/**"
---

# Checkout sessions (UCP and ACP)

Loads when working on a checkout session on either surface, its completion, its order events or the cart it holds. These are the invariants that keep one checkout to one order. The code wins over this page.

- **A checkout no session is stored for is not "nothing happened".**
  The id does not say whether the checkout was never completed, placed
  an order and is no longer kept (expiry, a flush or a restore, a
  release that changes the session key), or
  never existed. Every "no session" answer is `mcpsrv.errCheckoutGone`
  (UCP, `not_found`) or `acp.msgSessionGone` (ACP's 404): an order may exist, check
  before starting another checkout, with the order id if the caller
  holds one and the store's order confirmation if not. Never answer a
  missing session with only "start a new one". On UCP that text is the
  spec's error response, not a tool error (`ucp.CheckoutNotFound`,
  `ucp.status: error`, `not_found`, `unrecoverable`, no `id`: the
  schema is closed), whichever step found the session gone: `negotiated`
  turns `errCheckoutGone` into it.
- **A checkout session answers only on the surface that created it, and
  one a platform signed the creation of only to that platform.**
  `Session.Serves(checkout.Caller)` decides, on `Session.Protocol` and
  `Session.Platform` (the verified signer of UCP's `create_checkout`,
  empty for an unsigned create, which any caller of its surface
  reaches). A surface reads a session with
  `Store.Load(schema, caller, id)` and locks one with
  `Store.LockSession(schema, caller, id)`. A session the caller may not
  reach is `ErrNotFound` there, so it gets that surface's
  missing-session answer (above). `LockSession` reads the session
  before it writes the lock, which is keyed by the id alone, so a call
  that may not reach it takes no lock. A request that holds a session
  reads it again as its creator (`Store.Current`,
  `Flow.saveOverEvent`). Two readers reach a session
  through a record of the gateway's own, not a caller's id, and read
  either protocol with `Store.LoadAnyProtocol`: an order event, through
  its order link (`Flow.ApplyOrderEvent`), and a cart's holder
  (`Store.HolderOf`). `Store.Lock` reads nothing: it locks a session
  being created (`takeCart`) and a cart's holder being superseded.
  Whether a session takes an update or a cancel is `Session.Open` on
  both surfaces (`acp.refuseChange`, `mcpsrv.closedTo`); each keeps
  its own wording and status codes. A complete of a session that is
  not open is answered before the flow on both (`completeClosed`), so
  `Flow.Complete` only ever sees an open one.
- **A completion finishes without its caller.** `checkout.Flow.Complete`
  runs on the caller's context only until the order is about to be
  created. From the in-progress save on it is detached
  (`context.WithoutCancel`) and bounded by three of the Django
  client's timeouts (`completionSteps`), because Django has the order
  whether or not the caller's context is still live. That context ends
  when the platform disconnects (an ACP request's always; an MCP
  tool's only for a client on protocol 2026-07-28 or later,
  `PropagateRequestCancellation`), and on every surface and protocol
  when the server cuts its requests at the end of the shutdown drain
  (`serve.go`; an MCP tool's through `mcptool.Add`): the same three
  checks cover both. So no completion detaches after the cut. A call
  whose context has ended before the detach places no order; after a
  placed order it is answered "order placed, read it again" without the
  upstream read, with an info log line. The process does not exit under
  one: the shutdown's final window holds `checkout.CompletionTail`,
  the budget and the key settle and lock release after it
  (`completionTail`, `cmd/gateway`). `Flow.Complete` alone stores
  the session and the order link from the in-progress save on, and
  returns a `checkout.Placement`: `OrderMayExist` (the zero value)
  spends UCP's completion claim, `NothingPlaced` releases it; ACP's key
  follows its answer (`keptAgainstKey`, `.claude/rules/acp.md`). A
  completion refused before the in-progress save stores nothing. So
  the ACP handler and the UCP tool store what the call itself
  submitted before they call it, so that a refused call keeps the
  buyer's latest choice, and only for a session that is still open
  (`Session.Open`): ACP's buyer, and on UCP the payment method a
  complete names first, for a checkout that holds none, when it
  leaves the total as the checkout showed it. A UCP complete is a
  command: one that would change the method the checkout holds, or
  add a fee, is answered `incomplete` before any claim, and stores
  nothing (`.claude/rules/ucp.md`). After the flow they
  only map the error and settle their own key (UCP's completion claim
  by the placement, ACP's record by the answer once it is rendered),
  on a context the caller cannot cancel, bounded by the Django
  client's timeout; neither saves the session again.
- **One completion places a session's order, and no guard that expires
  decides which** (#84). Every guard on a checkout expires while its
  holder may still be working, because Django can take longer than any
  of them lasts. So nothing rests on one:
  - **A session is stored by compare-and-set** (`checkout.Store`,
    `saveScript`). Every save gives the session a new random
    `Session.Revision`; `Store.Save` writes only over the revision the
    request loaded, and answers `ErrSessionChanged`, having written
    nothing, when another request stored the session since or it is
    gone. `Store.Create` writes only where no session is. There is no
    unconditional write of a session. The delete of a placed one past
    its lifetime is the save script's own, under the same check.
    `Store.Discard` is the one delete outside it: it removes a session
    its create stored, or may have stored, and gave to nobody (the
    create's key turned out to be another request's, the create could
    not answer, or the store failed the create itself). No caller
    holds its id. A later create can have read it as its cart's
    holder: that create's cancel then loses its compare-and-set, and
    it looks again.
    The revision is random, not a counter or a time: two
    requests that read one state and save in the same clock tick would
    otherwise write the same bytes, and neither could tell its own
    write from the other's.
  - **The in-progress save is the fence.** `Flow.Complete` sends
    `POST /order` only after its save of `complete_in_progress` was
    accepted, so of all the completions that loaded one ready session,
    under any lock or key, one reaches Django. The others return
    `ErrSessionChanged` with `NothingPlaced`. The same check keeps a
    stale snapshot (an update or a cancel that ran past its lock, a
    refused order's reset) from being written over a session that is
    being placed or is placed.
  - **A lost save is answered like a busy session**: ACP 409
    `idempotency_in_flight` with `Retry-After: 2`, UCP
    `errCheckoutChanged` (a `conflict` tool error), the order-event
    push 503. The request is sent again and reads the session as it is
    then. `ErrSessionChanged` means nothing was stored in the session,
    and no order was sent. It does not mean nothing happened: an
    update has written its cart lines and coupon to Django by then, so
    UCP adds `checkoutNotUpdated` to that answer. A completion's last
    save can lose only to an order event, or to a read that stored what
    the order said (`rendered`), the other writers of a session in
    progress, which move it only to `completed`: the completion then stores its outcome over
    the event's (`Flow.saveOverEvent`), keeping `completed` if the
    event said the order is paid. A last save that still cannot be
    stored, after its order was placed, is `ErrSessionNotSaved` with
    the key spent, and does not wrap `ErrSessionChanged`. A save Redis
    failed is `ErrUnavailable`, answered "the checkout records are
    unavailable" (`errCheckoutUnavailable`, ACP 503), never with
    Django's wording. The in-progress save and the save after the
    order call each get one more attempt when Redis fails them, with
    the same write: a first that was stored and lost only its reply is
    recognised as this call's. When Redis fails both attempts of the
    in-progress save, the completion reads the session once
    (`Store.wasStored`): if it holds the save's revision, which no other
    write can carry, the save is this call's and the order is placed;
    if not, or if the read fails too, `NothingPlaced`. The read ends at
    the completion's budget like every Redis step in it, so one that
    starts just before the budget ends fails there and sends no order.
  - **The lock only orders requests** (`Store.Lock`, `ErrLocked`). It
    lasts `lockSteps` upstream timeouts: what a completion does under
    it when Django answers every call just inside its timeout. A
    request that runs longer loses the lock, never the fence.
  - **A lock and an idempotency key are both a `checkout.Claim`**: a
    key holding `inflight:<the request's token>` until it expires.
    `Settle` and `Release` act only on a key that still holds that
    token (`Settle` also on one nobody holds), so a request that
    outlived its claim never writes or deletes the next holder's. A
    claim lasts one upstream timeout more than the lock. A request
    that lost its claim answers by what it did (the order it placed,
    the session it saved): the session, not the key, tells the next
    holder.
  - go-redis sends a command again when its reply was lost, so each
    script reports its own earlier run as done: a claim finds its own
    token, a save its own revision, a settle the value it wrote.
- **No answer hands out a new checkout while an order for the old one
  may exist.** A session is open (`Session.Open`), canceled, or may
  have an order (`Session.MayHaveOrder`: being placed, awaiting its
  payment, completed). Decide on those, never on `OrderUUID` or one
  status:
  - A session that may have an order takes no update and no cancel, on
    either surface (`acp.refuseChange`, `mcpsrv.closedTo`): neither
    would touch the order.
  - It is kept `terminalTTL` once the stored session says an order
    exists or may (`Session.keptLong`, `Session.keptUntil`): placed, a
    day after `PlacedAt`, or until its `ExpiresAt` if that is later; its order call unanswered (`Session.Unconfirmed`,
    set by `Flow.place`), a day after that save. While it is held a
    caller is told what became of the checkout and a payment event
    finds it; afterwards its id is only "no session" (above). A
    session only marked in progress keeps an open one's half hour. The
    mark is written before Django is asked, and the save after the
    answer can fail: a session Django refused the order of must not
    read "an order may exist" for a day because Redis failed twice
    (the second attempt sends the same revision, so a first that was
    stored and lost only its reply counts as this save's). The
    price is the other outcomes of that same failure, and a pod that
    dies in between: an order placed or unanswered is answered from
    its session for half an hour, and as a missing session after. The
    stored value cannot tell them apart; Django could (#83, Django
    #637). `docs/agent-protocols-runbook.md` section 11 is the
    operator's side.
  - A complete of one answers what it is, before any lookup: the
    session when its order is placed (completed, or on UCP awaiting
    payment), in flight while it is being placed, canceled when it
    is. A session being placed is "in flight" only until
    `Flow.CompletionDeadline`, which for one stored unconfirmed is the
    save that said so; past it, both surfaces say the outcome
    was never recorded (ACP `order_outcome_unknown`, UCP
    `errOutcomeUnknown`, which says what a person can do), and that
    holds for a read whose render fails too (`renderBody`,
    `checkoutResult`). A UCP read that renders says the same in its
    text (`pending`), beside the status. It is offered no link back to
    its cart.
  - **A handler holds a snapshot, and answers from it only through its
    exit.** Once update, complete or cancel has loaded its session, the
    lock that keeps the snapshot current can lapse, and another request
    may store the session: a complete that places the order, a payment
    event. What Django then says about the cart (it is empty, a line is
    gone, the stock is short) is that request's doing, and the snapshot
    describes a session that no longer exists. So the body of each of
    the three cannot answer by itself. On ACP it has no
    `http.ResponseWriter` and returns an `answer`
    (`updateSession`, `completeSession`, `cancelSession`); on UCP it
    returns a `sessionAnswer`, not the tool's result. Only `finish`
    turns one into a response, so a way out added later is a `return`
    of one too.
    - `fromSnapshot` (UCP also `refusedBy`) is an answer made from the
      loaded session or from what Django said on its behalf: a
      refusal, a render, UCP's replayed key, ACP's retry of a change
      the session already holds. `finish` reads the stored
      session and gives it only while the revision is the loaded one.
      Otherwise the request is answered by what is stored, through the
      function that answers that state everywhere else
      (`completeClosed`, `refuseChange`; `closedTo`, `cancelClosed`);
      "changed by another request" for a session still open; the
      missing-session answer; or unavailable when it cannot be read.
    - `afterSave` is the answer of a request whose own save was just
      accepted, which proves its snapshot was the stored session: an
      update or cancel that stored, a completion past the in-progress
      save. It is given as it is. Using it without such a save is the
      one mistake the types do not stop.
    - Create and the reads hold no snapshot across a wait: they answer
      the session as they stored or loaded it then. Answers given
      before a session is loaded (the key, the lock, the tenant) are
      not about it.
    - An update reads the stored session once more before its first
      cart write (`Store.Current`), and writes nothing to the cart of a
      session stored since. That is a check, not a fence: Django's cart
      is outside the session's compare-and-set, so a completion that
      starts while an update's cart writes are under way can order a
      cart that update has half written. Only the lock stands against
      that.
  - **Order events** (`Flow.ApplyOrderEvent`), by the session the
    order's link leads to:
    - terminal: nothing;
    - naming no order: any event records the order, read from Django
      by the event's own uuid, which the order link vouches is this
      checkout's: its id, and its creation time, from which the
      session's day counts. An order Django no longer has is logged at
      warn and answered 204, and the session is left as it is: a
      session is never completed without its order. The record
      `Flow.place` writes beside the session (`Store.IndexOrder`:
      `ag:{schema}:cs:v6:{id}:order`, the order's uuid) is for calls on
      the session, not events: it can be lost (a flush or a
      restore);
    - the event's order status and payment status set the status
      (`Session.FollowOrder`), as a read does: a checkout awaiting its
      payment is `canceled` with its order (paid after the cancel
      included: the store refunds it), else `completed` once its
      payment is charged; one in progress whose order is paid is
      `completed`. A terminal checkout never moves: one completed at
      placement stays so when staff cancel the order. An event is a
      snapshot Django took, and the states it moves a session to are
      final, so one delivered late or out of order is still true.

    It takes no session lock. Its save is the compare-and-set, and a
    lost one is a 503 that Django's outbox sends again later (its
    backoff starts at ten minutes; `.claude/rules/ucp.md`, order
    webhooks).
    `Flow.PlacedOrder` reads the same record for a session in progress,
    so a call on one whose completion placed the order and could not
    save it is answered with the order (`placedUnsavedErr`,
    `writePlacedUnsaved`).
- **A checkout publishes when it ends, and is readable until then**
  (#108). `Session.ExpiresAt` is every UCP and ACP response's
  `expires_at`. It never moves earlier than a value already published
  (a platform stops polling at it), and the session is kept at least
  until it (`keptUntil`). Each save sets it to the session's own end
  (`Session.expire`): an open one's half hour, a placed one's day. A
  checkout awaiting its payment is the exception: its expiry is its
  order's `paymentDecidedBy` (`Session.AwaitDecision`), when Django's
  sweep will have decided the order, or the session's end when nothing
  will. The gateway holds no payment window, grace or sweep number.
  The UCP render of a placed checkout reads its order
  (`checkout.Pricing.Order`), and takes its status and expiry from it
  (`FollowOrder`, `AwaitDecision`); `checkoutResult` stores what
  changed before it answers (`rendered`, a compare-and-set): a
  checkout publishes only what is stored, one stored meanwhile is read
  and rendered once more, and one whose state cannot be stored is
  answered `order_placed` (`unrenderedErr`). An expired payment link is never
  replaced: `ResumePayment` mints only the first link, and only while
  the order's hold is ahead and the checkout does not tell the
  buyer to wait for the store (it reads the order;
  `ErrPaymentAwaitsStore` answers the checkout), and Django's
  `payment_window_expired` and `order_not_payable` are
  `ErrPaymentWindowClosed`, final for that order (UCP
  `checkout_closed`). A link it minted is stored over a read that
  stored the checkout meanwhile (`keepLink`), so the next complete mints no
  other. ACP has no checkout awaiting a payment.
- **A checkout's totals are Django's quote, and its order is placed at
  the total it showed** (#107). `checkout.ComputePricing` reads the cart
  for its lines and `POST /cart/quote` (`django.Client.Quote`) for every
  amount, with the very choices the order is placed with
  (`checkout.QuoteRequest`: delivery, pay way, region, email). The
  gateway adds nothing up of its own: the free thresholds, the promotion
  waiver, the carrier's weight and parcels are Django's. A checkout that
  has not chosen both a delivery and a pay way shows the rows the quote
  priced.
  - **A choice the store refuses is named, not priced.** A delivery the
    store does not ship that way or the cart outgrew
    (`shipping_unavailable`, `shipping_weight_exceeded`,
    `shipping_parcel_too_large`), or a pay way not offered for the
    delivery or gone (`pay_way_unavailable`, `pay_way_not_found`), is
    left out and the rest quoted again (`Pricing.DeliveryRefused`,
    `PayWayRefused`). Any other refusal of the checkout's data (an email
    the store does not take) leaves only the cart to price
    (`Pricing.InputRefused`), and an empty cart prices nothing. An open
    checkout is then rendered not ready, with an error naming what to
    correct (UCP `incomplete`, ACP `not_ready_for_payment`).
  - **A quote that cannot be read is an error**, never a total without
    a row: the render fails, and each surface answers unavailable (UCP
    `unavailable` with `retryAfterSeconds`, ACP 503). So is a quote in
    another currency than the store's, under which every amount is
    shown.
  - **The total shown is stored by the calls that hold the lock.** Every
    render of an open checkout sets `Session.ShownTotal`
    (`Session.Show`), and the calls holding its lock store it: UCP's
    `rendered` with the status and expiry, ACP's `storeShown`, a create
    with its session. A read holds no lock and does not store it
    (`mcpsrv.unlocked`, ACP's GET): a read that stored it would make the
    update or completion holding the lock lose its save. A read showing a
    newer total than stored, or a store that fails, costs one
    `total_changed`: the order sent at the total stored is refused, and
    that answer, under the lock, stores the new one. An open checkout
    that other requests keep storing is answered as changed (UCP
    `conflict`), never as a placed order.
  - **The order is placed only at it.** `Flow.admit` refuses a checkout
    that has shown no total (`ErrTotalNotShown`, which is `ErrNotReady`:
    read it, then complete), and one whose choices the store's quote now
    refuses, whatever its stored status (`ErrChoiceRefused`, also
    `ErrNotReady`). `Flow.place` sends the shown total as
    `expectedTotal`. Django creates the order only at exactly that
    amount due, and answers another with 409 `total_changed`
    (`django.ErrTotalChanged`): nothing
    was created, the checkout is stored ready again, and the surface
    answers with the total now (UCP: the checkout, `incomplete` for that
    answer, with a `recoverable` error `total_changed`, its claim
    released; ACP: 400 `total_changed`, the next read showing the
    total). So no order is ever placed at a total its checkout did not
    show.
- **One live checkout holds a cart** (#141). Two sessions on one cart
  could each place an order, because Django keeps the cart of an order
  that awaits its online payment. The store keeps it to one:
  - `ag:{schema}:checkoutcart:{cartId}` names the session that holds
    the cart. `Store.Create` stores the session and takes the cart in
    one script (`saveScript`): when no session holds the cart, or the
    record still names the holder the caller read (`Store.HolderOf`)
    and that holder is free (`CartHolder.Free`: no longer stored,
    canceled or completed). A cart another session holds is
    `ErrCartHeld`, and nothing is written.
  - The record lives as long as its session: every save sets its
    expiry to the session's, and the save that ends a session and
    `Store.Discard` delete it, each only while it names that session.
    A save writes the record again where Redis holds none (lost to a
    flush or a restore). A record whose session is not stored holds
    nothing.
  - UCP `create_checkout` with `cart_id` (`mcpsrv.createFromCart`)
    takes a UUID only, in its canonical spelling
    (`checkout.CanonicalCartID`: Django reads every spelling as one
    cart), and reads the holder before any lookup or cart write
    (`cartHolder`). **The newest create holds the cart: an open UCP
    checkout on it is canceled**, with `cancel_checkout`'s own save
    (`handlers.cancel`), and the new session takes its place; the old
    id reads `canceled` from then on. The create's region and payment
    are settled first, so a create the store refuses cancels nothing.
  - **A cart is written only by the checkout that holds it, in that
    checkout's turn.** Django's cart is outside the session's
    compare-and-set, so only a lock keeps one checkout's cart writes
    out of the next checkout's cart. The holder is canceled under its
    own lock (`supersede`): a call at work on it (an update writing
    the cart, a completion) holds that lock, and the create does not
    cancel under it. The create's discount codes are written after its
    checkout took the cart, under the new checkout's lock, which is
    held from before the checkout is stored, so the next create waits
    for that write. A create that fails after it took the cart (the
    code write, the save after it, the render: `handlers.created`)
    discards the new checkout (`handlers.discard`) and leaves the old
    one canceled: the cart then has no checkout. A `line_items`
    create that cannot render is discarded the same way: no create
    leaves a checkout nobody was given.
  - A holder whose order is being placed or awaits its payment keeps
    the cart, and so does an open checkout the caller may not reach
    (another protocol's, or one another platform signed the creation
    of): the create is a `conflict` (`errCartOrderPending`,
    `errCartOrderUnpaid`, `errCartElsewhere`). The first two say that
    an order exists or may, tell the caller not to start another
    checkout, and say where to look with the checkout id or order id
    it already holds.
  - **An order that awaits its payment is asked of Django, not of the
    session** (`orderCanceled`, `django.Client.PlacedOrder`). Django
    cancels an unpaid online order when its payment window ends and
    keeps the cart, while the session reads "awaiting payment" for its
    whole day: only a payment changes it. A checkout whose order
    Django canceled is canceled as an open one is, and the create
    takes the cart, so a shopper who left the payment page can check
    out again, whatever protocol that checkout was (nothing is left
    of it to finish). An order Django no longer has keeps the cart;
    one that cannot be read answers `unavailable`. Two states fail
    closed: a checkout still in progress that names a canceled order,
    for the session's day (`FollowOrder` moves one in progress only to
    `completed`), and a paid order whose payment event never arrived,
    until a read or an event of the checkout completes it. The holder
    check reads the order only for "canceled", and stores nothing.
  - **No answer to a create gives another checkout, or names one or
    its order.** A checkout created unsigned is bound to no platform,
    so the UCP cart capability's "return the existing checkout" is not
    offered.
  - A create that finds the holder changed by another request (a
    cancel that lost its save, a cart another create took), or a call
    at work on it, decides again on what the holder is then,
    `cartCreateRounds` times at most, then answers `conflict`
    (`errCartBusy`).
  - A cart made for its session (`line_items`, ACP) is taken the same
    way. A held one there is a fault: logged, answered unavailable,
    and no session is handed out.
  - Redis can lose the record while the session lives (a flush or a
    restore). Every load of a session touches its cart's record
    (`Store.Load`, `Store.LoadAnyProtocol`), and a session's next save
    writes a lost record again. A session nobody reads or saves, such as one
    left awaiting its payment, can still lose it, and the cart can
    then get a second checkout: the gateway cannot close that alone.
    What holds whatever Redis lost is Django, which refuses an
    agent's order from a cart whose earlier order still awaits its
    online payment (django-api #693). Its `code` is named once,
    `django.CodeCartHasUnpaidOrder`, and `CreateOrder` reports it as
    `django.ErrCartHasUnpaidOrder` (still Django's `*APIError`, so
    a refusal: nothing placed, the session left `ready_for_complete`;
    UCP's completion claim is released, ACP's key keeps the refusal
    like any other). Both surfaces branch on that sentinel. UCP
    answers Django's code and sentence with its own hint
    (`cartOrderUnpaidErr`): this checkout placed no order, start no
    other checkout, and once the store cancels that order unpaid this
    checkout can be completed (once the buyer pays it, Django empties
    the cart and the next complete is refused `cart_empty`); the
    counter reads it as `refused`. ACP answers 400 `invalid_request`, code
    `cart_has_unpaid_order`, with the gateway's own message of the
    same meaning (`completeError`). Neither names an order: Django
    names none.
  - **A cart Django no longer serves is gone, not a render to retry,
    and not "nothing happened".** Django removes a guest cart that an
    order emptied or a sign-in merged, so the shopper may have checked
    it out on the store's own site. The render of a checkout without
    an order reads its cart, so a cart Django no longer serves there
    (`isCartGone`: another cart served, `cart_not_found`, or a 403) is
    `errCheckoutCartGone`, `not_found` with `cartGone`, never "retry";
    an outage stays `unavailable`. A `cart_id` create answers it with
    `cartIDGone` and "No checkout was created" (`handlers.created`),
    and leaves nothing stored. Both say an order may exist and to
    check with the shopper before starting another checkout or cart:
    never only "start a new one".

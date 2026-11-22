---
paths:
  - "internal/mcptool/**"
  - "internal/mcpsrv/**"
  - "internal/adminmcp/**"
  - "internal/chat/bridge.go"
---

# MCP tools (`internal/mcptool`, and every tool on /mcp, the chat and /mcp/admin)

Loads when working on a tool of any of the three MCP servers. The code wins over this page.

- `internal/mcptool` — what the tools of both MCP servers answer with:
  `ToolError` and every code the gateway mints, the `Untrusted` marker
  (`MarkUntrusted`) for text a customer wrote, with the sentence a
  tool's description adds when it returns such text (`UntrustedNote`),
  and `Add`, through which every tool on /mcp, the chat and /mcp/admin
  is registered. On /mcp and the chat, `get_product_reviews` marks what
  the reviewer typed (their name and the comment): the chat reads it
  into a model that holds cart tools.
  - `Add` logs one info line per call that reaches the handler, "tool
    call", with `server` (`mcptool.ServerMCP`, `ServerChat` or
    `ServerAdmin`), the tool, the outcome (ok, the code, error or panic)
    and the duration. The context handler adds the request id and the
    store.
  - It never logs the arguments or the result.
  - It counts the call on `gateway_tool_calls_total{server,tool,outcome}`.
    The outcome is the gateway's own: a code Django or a coupon refusal
    gave is counted as `refused` (`mcptool.OutcomeRefused`), the log line
    keeps it.
  - An unknown tool, and arguments that fail the input schema, are
    refused by the SDK before the handler, neither logged nor counted.
  - A POST to /mcp or /mcp/admin is read once by `mcptool.ReadBody`
    (bounded as the SDK bounds it) before anything decodes it. A
    JSON-RPC batch is refused there with a 400 when the request's
    `Mcp-Protocol-Version` is 2025-06-18 or later, which removed
    batching (the SDK refuses one too, but only after decoding it), and
    where the protocol still has batches (2025-03-26, 2024-11-05, or no
    header, read as 2025-03-26) when it holds more than
    `MaxBatchMessages` (8, why in its comment).
  - A handler panic is recovered there (`.claude/rules/httpmw-obs.md`).
  - Each handler's context ends at the server's shutdown cut, with
    `httpmw.ErrShuttingDown`, on any protocol (`.claude/rules/shutdown.md`).
  - Every tool's annotations are `mcptool.ReadOnly`, `Adds`, `Replaces` or
    `Changes`, none open-world, each with the tool's title, which `Add`
    also sets as `Tool.Title` (a 2025-03-26 client, which the transport
    assumes for a request naming no version, reads only
    `annotations.title`).
  - `mcptool.Cacheable` lets a client keep `tools/list` for
    `TENANT_CACHE_TTL`: public on /mcp, private on /mcp/admin. A pod may
    serve a store's features up to about two TTLs old (a Redis hit gets
    a fresh memory TTL), so a client can list a switched feature's tools
    for about three TTLs. A call is still gated on the server: by the
    pod's copy of the store's features, then by Django's own gates.

  How a Django failure maps to a code is each server's own `upstreamErr`.

## How a tool fails

- Business failures in MCP tools surface as `CallToolResult{IsError:
  true}` with actionable text. A Django failure goes through
  `upstreamErr`, which renders an `mcptool.ToolError` as JSON text:
  Django's `code` and its `detail` (or its validation `fields`) for a
  refusal, `retryAfterSeconds` for a throttle, and a `hint`. A 403 is
  reported as `not_found` on /mcp and the chat, and as `not_allowed` on
  /mcp/admin, where the person needs to know it is their role
  (`.claude/rules/adminmcp.md`). A 5xx or network failure is only
  `unavailable`: no upstream text beyond what Django wrote for a client.
  The other failures an agent acts on by their kind are `ToolError`s
  too: no linked account (`unauthorized`), a token without the tool's
  scope (`not_granted`, as on `/mcp/admin`), a refused coupon (its
  reason as the code), the checkout store down
  (`errCheckoutUnavailable`, one value, `retryAfterSeconds` as an
  upstream outage). Every refusal of the UCP checkout tools is one,
  named as ACP's `Error` codes name the same state:
  - arguments the input schema cannot refuse: `invalid`
    (`invalidInput`);
  - a checkout no session is stored for: `not_found`
    (`errCheckoutGone`), answered as the spec's error response, not a
    tool error (`negotiated`, `ucp.CheckoutNotFound`);
  - a checkout another request holds the lock of (`errCheckoutBusy`,
    send again shortly) and one whose completion may still be running
    (`errCompletionInFlight`, read it, never complete again):
    `conflict`;
  - a checkout canceled or with its order placed (`closedTo`), for
    good: `checkout_closed`, never retried. UCP defines no code for it
    (`error_code.json`), and ACP answers it with its 405;
  - not ready to complete, or no payment method: `missing`;
  - a payment method the delivery or this surface cannot take:
    `unsupported`;
  - not enough stock: `out_of_stock`;
  - a total that changed since the checkout showed it:
    `total_changed`, and a cart that changed since, at the same total or
    not: `cart_changed` (read the checkout, which shows it now, see
    `.claude/rules/ucp.md`);
  - an order placed whose checkout was not saved or not rendered:
    `order_placed`; an order the store has not confirmed:
    `order_outcome_unknown`, with `retry_after`, whose hint says to
    complete the same checkout again (`checkout.Flow.Resume`); an order
    whose payment link could not be made: `payment_link_failed`. Each
    hint says not to start another checkout.

  A cart tool's failure goes through `handlers.cartErr`,
  or `handlers.cartGoneErr` for a call on the cart itself (a line's or a
  product's 404 stays theirs). There a 404 with Django's code
  `cart_not_found` (`django.CodeCartNotFound`) or a 403 is the cart
  gone; a bare 404 is a feature gate's, answered `not_found` with
  `featureOff` on both surfaces. A cart the store no longer serves
  (`django.ErrCartUnreachable`, or the cart gone) is `not_found` on
  `/mcp`, whose agent owns its cart id and is told to
  create another with `create_cart` (`mcpCartGone`, for a call on
  the cart itself, and `add_to_cart`'s own wording).
  `update_cart_item` and `remove_cart_item` are `not_found` there
  too, but keep their own hint, "that cart line was not found;
  fetch the cart with get_cart" (`cartErr` with the line's
  not-found). In the chat it is the shopper's
  own browser cart, which a cart the model created would never replace
  and which the chat serves no `create_cart` for, so it is
  `cart_unreachable`: nothing changed, tell the shopper to reload the
  page and try again (`shopperCartUnreachable`, the same answer as a
  refused cart token). A checkout write that failed after
  another landed says so on top of its cause's hint (`withHint`). A list
  with nothing in it is `[]`, never `null`
  (`TestToolOutputsListNothingAsEmptyArrays`), and every output schema
  declares a list an `array`, not `["null","array"]`, on /mcp, the chat
  and /mcp/admin (`mcptool.OutputSchema`, which `mcptool.Add` gives a
  tool without a schema of its own): the SDK checks each result against
  it.
  `find_pickup_points` reads Django's 400 from BOX NOW's nearest lookup
  as no locker found: Django answers every BOX NOW refusal with it.
  Each point it finds carries its `destinationId`
  (`ucp.PickupLocation.DestinationID`), naming the point's own country
  (a BOX NOW locker BOX NOW gives none for, the country searched), which a UCP checkout's pickup
  method takes as `selected_destination_id`.
  Returning a plain Go error from a typed tool handler is fine — the
  SDK wraps it into exactly that; only a `*jsonrpc.Error` becomes a
  protocol error, so never return one for a business condition. The single exception is UCP discovery: a platform
  profile that cannot be fetched, validated or version-matched is the
  spec's transport error `-32001`, and a spent discovery budget (the
  shared one or the caller's share) is its retryable `-32000`, with
  `retry_after` the seconds until that budget has a token again
  (`ucp.DiscoveryRetryAfter`, `mcpsrv.negotiate`).

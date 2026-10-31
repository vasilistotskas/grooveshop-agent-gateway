---
paths:
  - "internal/ucp/**"
  - "internal/mcpsrv/ucp_*.go"
  - "internal/mcpsrv/order.go"
  - "internal/httpsig/**"
---

# UCP conventions

Moved from `CLAUDE.md`; loads when working on the UCP packages. The code
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
  The order webhook URL comes ONLY from the negotiated order
  capability's `config.webhook_url`, and `pay_way_id` needs the
  hosted-selection extension negotiated, not just the tenant gate.
- **The canonical UCP tools take the wire shapes from the OpenRPC document**
  (`create_checkout`, `get_checkout`, `update_checkout`,
  `complete_checkout`, `cancel_checkout`): `meta` (with
  `ucp-agent.profile`, plus `idempotency-key` on complete/cancel), `id`
  for the target session, and `checkout` for the domain object with
  snake_case members. Extensions compose FLAT onto `checkout`
  (`fulfillment`, `discounts`), never nested under capability names.
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
  selection. These five tools are withheld from the
  chatbot (`internal/chat/bridge.go`) — their shapes target a platform
  generating calls, and chat hands over a checkout link instead.
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
  and `UCP-Agent` naming the tenant profile. The queue is a Redis stream
  consumer group (`ag:webhooks:orders`, one consumer per pod name): a
  delivery is acknowledged only on a terminal outcome, a consumer's own
  pending entries replay on restart, and a dead consumer's are claimed
  after the visibility timeout. Delivery dials only public addresses and
  never follows redirects.

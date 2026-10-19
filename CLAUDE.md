# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with
code in this repository.

## Project Overview

GrooveShop **agent gateway** — a Go microservice that makes every tenant
storefront AI-shoppable. One binary serves five public protocol surfaces on
the storefront's own domain (Traefik path-routes them here), plus a
cluster-only internal endpoint and the operational routes:

| Path | Surface |
|---|---|
| `POST /mcp` | MCP server (stateless streamable HTTP): the UCP checkout capability's five canonical tools + ergonomic commerce tools + account tools (`my_orders`, `my_loyalty_points`, `my_favourites`); a tool whose store feature is off is neither listed nor callable |
| `GET /mcp/server-card` | MCP Server Card (SEP-2127): identity and remotes only — no tools, which clients read from `tools/list`. Name/title/version match `initialize` (`mcpsrv.IdentityFor`) |
| `GET /.well-known/ard.json` | Agentic Resource Discovery manifest (ARD v0.91, `application/json`): one entry for the MCP server, with the card's title/description, the tool names the store's `tools/list` serves as `capabilities` and `representativeQueries` |
| `GET /.well-known/ai-catalog.json` | AI Catalog (`application/ai-catalog+json`): the same entry as `ard.json`, name and description included, because Lighthouse validates this document against the ARD schema |
| `GET /.well-known/ucp` | UCP business profile (spec 2026-08-25), incl. the `space.grooveshop.payments` handler |
| `/acp/*` | ACP agentic checkout REST (spec 2026-04-17) |
| `/feeds/*` | Product feeds: google.xml, meta.xml, tiktok.xml, acp.json |
| `POST /chat` | First-party shopping chatbot (SSE; OpenAI-compatible protocol via openai-go — Gemini free tier by default, any compatible provider via CHAT_BASE_URL) |
| `/internal/*` | Cluster-only: Django order-event push (shared-secret header) |
| `GET /healthz`, `GET /readyz` | Liveness / readiness (readiness gates on Redis only; Django is reported, never gating) |
| `GET /metrics` | Prometheus metrics (registry from `internal/obs`) |

The gateway is a **protocol adapter**: all commerce state lives in Django
(REST API, camelCase), sessions/caches live in Redis. It holds no database
and no PSP secrets.

## Commands

```bash
go build ./...                                   # Compile
go test ./internal/...                           # Unit tests
go test -tags integration ./internal/integration/...  # Docker-backed tests
golangci-lint run                                # Lint (v2 config)
go run ./cmd/gateway                             # Run (needs .env vars exported)
docker build -t agent-gateway .                  # Production image
```

CI runs lint → unit+integration (with `-race`; local Windows Go is 386 and
cannot race-detect) → build.

The `/gateway-test` skill wraps these. `.claude/` also registers a
`gateway-contract-reviewer` subagent for the invariants below, and hooks that
`gofmt` and `go vet` each edited file.

## Architecture

- `cmd/gateway` — wiring only: config → clients → mux → graceful shutdown.
- `internal/config` — env config, fail-fast `Load()`: unknown `ENV` or
  `LOG_LEVEL` values refuse to boot, and `ENV` (production | staging |
  development | test) is folded into `AllowLocalWebhooks` and
  `PaymentHandlerEnv` there so no handler compares environment strings.
  The infra staging overlay sets `ENV=staging`: production network
  rules, sandbox handler label.
  Per-tenant values come from Django `tenant/resolve` at request time,
  never env.
- `internal/identity` — optional-auth middleware on /mcp: a present
  `Authorization: Bearer` is verified against Django `/agent/me`
  (60s in-memory cache, SHA-256 keys); invalid tokens get 401 + the
  RFC 9728 `WWW-Authenticate` challenge; anonymous requests pass
  through. Account tools forward the bearer — Django enforces scopes.
- `internal/tenant` — resolver (process memory 5m → Redis `ag:tenant:v2:{host}`
  → Django; singleflight; 60s negative cache; stale-if-error) + middleware
  (tenant from the **real Host header only** — inbound X-Forwarded-Host is
  deliberately ignored; spoofing it must never switch tenants).
- `internal/django` — typed API client. Every tenant-scoped call sends
  `X-Forwarded-Host: <tenant domain>` + `X-Forwarded-Proto: https` (+
  `X-Language`). GETs retry twice on 5xx/network; mutations never retry at
  transport level. Errors unwrap to sentinels (`ErrNotFound`, `ErrConflict`,
  `ErrThrottled`, `ErrValidation`, `ErrUpstreamDown`) — branch with
  `errors.Is`, never on status literals. Django never refuses a cart an
  anonymous caller cannot reach (a signed-in shopper's, or a guest cart
  since deleted): it answers 200/201 with a new guest cart and acts on
  that one. Every cart call that names a cart therefore checks the cart
  that answered (`servedNamed`) and returns `ErrCartUnreachable` for
  another. A signed-in shopper's cart is reached only through the cart
  token the chat widget forwards (`django.CartToken`, in the request
  context): the cart routes Django accepts it on (`cartTarget`) send
  `X-Cart-Token` instead of `X-Cart-Id` and trust the cart it resolves
  to; a 401 there is `ErrCartTokenRefused`. Stock, order and payment
  calls never carry it.
  A call made while serving a request names that request's caller, as
  the storefront's SSR proxy does (`django.RelayCaller`, mounted in
  `server.New`): `CF-Connecting-IP` as `X-Real-IP`, `X-Forwarded-For`,
  `X-Origin-Verify` and `User-Agent`, relayed as received. So Django's
  throttles and `Idempotency-Key` scopes see the visitor, not the pod.
  The gateway decides and mints nothing here: Django believes the
  address only beside a correct `X-Origin-Verify` (`core/client_ip.py`),
  and the inbound `X-Real-Ip` (Traefik's own peer) is never relayed.
- `internal/httpmw` — recover, request-id, access log, RED metrics, per-IP
  rate limit. Metrics wraps the mux directly so `r.Pattern` is populated;
  tenant middleware runs *inside* routing (per route group), and enriches
  the access log via `httpmw.SetTenant` (the `Extras` holder).
- `internal/obs` — slog JSON to stdout, Prometheus registry, health.
  `/healthz` is process-only; `/readyz` gates on Redis ONLY — Django-down
  must never flip readiness (agents get actionable errors, not 502s).
- `internal/server` — mux assembly; new route groups register here.
- `internal/storefront` — every link into the Nuxt storefront (product,
  cart claim, order success, legal pages, OAuth metadata). The paths are
  a cross-repo contract with the Nuxt `app/pages` and `server/routes`
  trees; build URLs here, never with a `"https://" + domain + path`
  literal in a renderer.
- `internal/text` — rune-safe truncation. Byte-index cuts split Greek
  runes; use `text.Runes` / `text.Ellipsize`.
- `testdata/fixtures/django/` — recorded upstream responses for the DTO
  decode tests. They are hand-recorded, so they prove decoding, not that
  Django still sends a field: that is the consumer contract's job (below).

## Conventions

- Hand-written minimal DTOs (camelCase tags); decode with `UseNumber()`,
  money as `json.Number` → integer cents internally, format at the edge.
- Business failures in MCP tools surface as `CallToolResult{IsError:
  true}` with actionable text. A Django failure goes through
  `upstreamErr`, which renders an `mcpsrv.ToolError` as JSON text:
  Django's `code` and its `detail` (or its validation `fields`) for a
  refusal, `retryAfterSeconds` for a throttle, and a `hint`. A 403 is
  reported as `not_found`, and a 5xx or network failure only as
  `unavailable`: no upstream text beyond what Django wrote for a client.
  Returning a plain Go error from a typed tool handler is fine — the
  SDK wraps it into exactly that; only a `*jsonrpc.Error` becomes a
  protocol error, so never return one for a business condition. The single exception is UCP discovery: a platform
  profile that cannot be fetched, validated or version-matched is the
  spec's transport error `-32001` (`mcpsrv.negotiate`).
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
- **A store feature switched off is off on every surface.** Django
  folds each into one boolean, the plan flag AND the store setting, under
  `features` on `tenant/resolve` (`TenantConfig.Features`); read
  nothing else. A tool serving a feature is registered with `onlyWith`
  (`internal/mcpsrv/server.go`): `get_product_reviews` needs
  `productReviews`, `my_loyalty_points` `loyalty`, `my_favourites`
  `favourites`. The per-store server (cached per feature set) neither
  lists nor serves it, discovery does not advertise it, and the chat
  bridge, which shares one server built with `EveryFeature`, filters
  through `mcpsrv.Offered`. With reviews off no rating or review count
  leaves the catalog tools either (ratings are on Django's 1–10 scale).
  With promotions off `checkout.ApplyDiscountCodes` refuses every code
  as `discount_code_invalid` without applying it (Django answers 404
  with the plan flag off, which would fail the request) and clears the
  cart's old coupon best effort, and ACP sessions do not declare the
  discount extension.
- Redis keys: `ag:{schema}:…`; tenant cache `ag:tenant:v2:{host}`,
  versioned with the cached `TenantConfig` shape: bump it when a field is
  added, or entries an older pod wrote decode it as its zero value.
- No tenant label on Prometheus metrics (unbounded cardinality) — tenant
  goes in logs.
- **No fallback/legacy/backward-compat code paths.** Environment differences
  are config values, not conditionals.
- Multi-tenant native: **no tenant name appears in gateway code**, not
  even the first one. Tenant identity is the `schema` carried on the
  request; anything keyed by it (Redis keys, signing keys, prompts) is
  derived, never enumerated. Never hardcode production domains — they
  arrive via tenant config. Tests use `demostore`/`acme`/`alpha`, and a
  literal schema name in non-test code is a bug.
- 80-char-ish lines, comments only for non-obvious constraints.

## Django consumer contract

`encoding/json` zero-fills a field Django stops sending, so a removed or
renamed field would serve wrong data (0 prices in feeds) with no error.
`contract/django-consumer.openapi.json` is the OpenAPI 3.0.3 document of
what the gateway needs from Django: every endpoint it calls, the query
parameters and bodies it sends, and only the response properties its DTOs
decode. It is the gateway's side of the infrastructure deploy gate
(`api-contract.sh`): `oasdiff breaking <contract> <Django schema.yml>`
for each gateway/Django pairing, as for the storefront, refusing a Django
release that drops, loosens or retypes one of those properties.

- **Generated, never edited.** `internal/django/contract_test.go` holds the
  call registry (client method, method, path, success status, query
  parameters, body/response DTO types, Django component names) and builds
  the document by reflecting over the DTOs. After changing a DTO or a call:
  `go test ./internal/django -run TestDjangoConsumerContract -update`, and
  commit the file; without `-update` a stale file fails the test.
- **Every call is registered.** `TestDjangoContractCoversEveryCall` parses
  the `client*.go` sources and fails when a `c.get`/`c.send` call is missing
  from the registry or differs from it (path, method, query keys and
  whether each is always sent, body and response types). Paths must be
  templated string literals (`/product/{id}`, concrete one in `rawPath`).
- **Required/nullable** come from the Go type: a value field is required
  and non-null; a pointer is optional, and nullable unless `omitempty`;
  `omitempty` is optional. Where Django's schema is looser than the Go type,
  the field says so with a tag the readers already honour (they treat the
  zero value as "not set"): `contract:"optional"`, `contract:"nullable"`,
  `contract:"untyped"` (Django declares a `oneOf` union),
  `contract:"undocumented"` (read but absent from Django's schema — e.g.
  the internal-only `chatApiKey`). A sent value carries Django's declared
  format with `contract:"format=email|uri|double"`, and its Go type must
  marshal to Django's JSON type (`targetPrice` is a `json.Number`, sent as
  a number). Never tag a field to silence a real break: fix the DTO or the
  call.
- **How it is read.** oasdiff needs `--flatten-allof` (Django nests objects
  as `allOf`) and `--severity-levels contract/oasdiff-levels.txt`: removing
  an *optional* read property is an error too, and so is a request body
  property changing type or format. Informational only: enum values and
  unions (Django's to grow), request lengths/ranges/enums (Go types cannot
  state them), and query/path parameter types — parameters are declared as
  strings on purpose, since that is what the URL carries and Django's
  integer parameters accept it. Check a pairing locally with:

  ```bash
  docker run --rm -v "$PWD/contract:/c" tufin/oasdiff:<pinned> breaking \
    --flatten-allof --severity-levels /c/oasdiff-levels.txt \
    --allow-external-refs=false --fail-on ERR -- \
    /c/django-consumer.openapi.json \
    https://raw.githubusercontent.com/vasilistotskas/grooveshop-django-api/<tag>/schema.yml
  ```

## Cross-repo contracts

- Django repo (`../grooveshop-django-api`): `tenant/resolve` endpoint shape,
  cart `X-Cart-Id` UUID header, guest checkout, `create_checkout_session`
  (Viva Smart Checkout URL). Payments: **Viva primary** (hosted
  authorization via UCP `requires_escalation`+`continue_url`), Stripe
  tokenized behind per-tenant flag.
- Nuxt repo: `/cart/claim?uuid=` claims a gateway-built cart into the
  browser session;
  `.well-known/oauth-protected-resource[/mcp]` names the Django API as
  the OAuth authorization server (allauth.idp: auth-code + PKCE + DCR).
- Agent discovery is the gateway's alone: `/.well-known/ard.json` and
  `/.well-known/ai-catalog.json` are both cut from one entry
  (`mcpsrv.storeEntry`), so they cannot drift from each other or from
  the Server Card. `identifier` is `urn:air:<primary domain in
  ASCII>:mcp:store`; `capabilities` come from the same `toolset` that
  `NewServer` registers for the store's features (`toolNamesFor`), so a
  new tool is advertised automatically and a switched-off one is not;
  `representativeQueries` sit beside the tool definitions. Both share
  `/mcp`'s agent-commerce gate (a store without it gets the same 404),
  and the Server Card's CORS, ETag and cache headers.
- Nuxt chat widget (`app/composables/useShopChat.ts`) reads the `/chat`
  SSE events: `delta {text}`, `tool {name, status: running|done|error}`
  (`error`: the call failed, and the model was told why),
  `products {tool, query?, total?, products: [{id}]}`,
  `error {message, code?, retryAfter?}` (`code: rate_limited` when the
  model provider throttled the turn, `retryAfter` its wait in seconds),
  `done {conversationId, cartId?, cartMutated, cartTokenRefused}`.
  The request is `{message, conversationId?, cartId?, cartToken?,
  turnId?}`. `turnId` is a UUID the widget mints per message and sends
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
  only place B2B pricing applies (see below).
- Config gates: ACP and chat are per-tenant. The ACP platform bearer
  arrives as `acpBearerToken` and the chat model credential as
  `chatApiKey` on `tenant/resolve` — Django includes both only when the
  resolve call carries the gateway's `X-Internal-Token` (same shared
  secret as the order-event push). A tenant without a chat key gets a
  localized 404 from `/chat`; a tenant without an ACP token has ACP
  disabled — every bearer gets 401 from `/acp/*`.
- Chat spend is bounded in three layers:
  - Traefik's `rate-limit-chat`, per IP.
  - The gateway's per-pod `(host, IP)` limiter (`CHAT_RATE_LIMIT_*`).
  - The Redis turn quota in `internal/chat/quota.go`, shared by every pod and counted only for valid turns, before any model call:
    - `CHAT_STORE_TURNS_PER_HOUR` per store per clock hour;
    - `CHAT_VISITOR_TURNS_PER_DAY` per client IP per store per UTC day.
  
  A refused turn spends from neither budget and gets the store's localized `rate_limited` message with a 429 and a `Retry-After` until the full window resets.
- Infra repo: manifests under `manifests/app-constructs/grooveshop/base/`,
  path rules on the storefront ingress.
- B2B/wholesale pricing (Django `b2b/` app) is RETAIL-ONLY here by
  design: every gateway catalog/cart/checkout call is anonymous (no
  bearer forwarded), so MCP tools, UCP/ACP checkout and feeds always
  see retail prices — totals stay self-consistent end to end. The one
  exception is a chat turn's cart calls with a cart token: Django prices
  that cart for its owner, so a wholesale shopper's cart shows their
  prices. Only
  `/api/v1/agent/*` forwards agent bearers, and the only current price
  they carry (favourites' `effectivePrice`) is the product's stored
  retail one. Do not thread identity into the catalog/cart clients for
  B2B without redesigning the feed/catalog caching (one artifact per
  tenant assumes one price per product).

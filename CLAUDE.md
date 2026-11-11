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
| `POST /mcp/admin` | Store-admin MCP server (`internal/adminmcp`): the store's own team reads orders, sales, profit, products and stock, customers, reviews and contact messages, by role, read only. Every call needs a staff bearer from Django's staff issuer on `STAFF_CONNECT_HOST`; `GET /.well-known/oauth-protected-resource/mcp/admin` names it |
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
go test -tags realstack ./internal/realstack/...      # A real Django; see below
golangci-lint run                                # Lint (v2 config)
go run ./cmd/gateway                             # Run (needs .env vars exported)
docker build -t agent-gateway .                  # Production image
```

CI runs lint → unit+integration (with `-race`) → build. `-race` requires
cgo; the local Go builds with `CGO_ENABLED=0` and has no C compiler, so
`go test -race` fails at once with `-race requires cgo`. Run it in CI.

The `/gateway-test` skill wraps these. `.claude/` also registers a
`gateway-contract-reviewer` subagent for the invariants below, and hooks that
`gofmt` and `go vet` each edited file.

The real-stack test (`internal/realstack`) runs the gateway image against a
real Django, its database and its search, including a real sign-in. It takes
about 12 minutes, so `.github/workflows/real-stack.yml` runs it before a release,
nightly and when the test changes. Locally it needs `REALSTACK_DJANGO_IMAGE`
and `REALSTACK_GATEWAY_IMAGE`, and a lot of memory.

## Releasing

A release is a pushed `vX.Y.Z` tag. Before tagging, get an independent review of
everything since the last tag (`git diff <last tag> origin/main`), briefed with
the code and not with your conclusions, and read it before you tag. CodeRabbit
and PR Lens read one pull request at a time and have passed bugs a full read
found. A test written with its fix does not prove the contract: check the other
side's code. `release.yml` also refuses to publish unless the real-stack test
passes.

## Architecture

- `cmd/gateway` — wiring: config → clients → mux, then `serve`
  (`serve.go`), the one piece of logic, tested. At SIGTERM (after the
  Deployment's 10s preStop sleep) in-flight requests drain for 10s with
  live contexts; then every request context is cancelled with cause
  `httpmw.ErrShuttingDown`, and a stream gets 5s to write its last
  event (the chat sends its `turn_failed` `error`). What is still open
  is closed, logged at warn, and the process exits 0: 25s, inside
  `terminationGracePeriodSeconds: 30`. `ReadTimeout` (30s, half of
  Traefik's entrypoint `readTimeout`) bounds reading a request; the
  server clears it once the body is read, so streams outlive it.
  `IdleTimeout` (120s) sits above Traefik's 90s backend idle timeout.
  No `WriteTimeout`: it would cut every stream.
- `internal/config` — env config, fail-fast `Load()`: a value that would
  boot and then misbehave refuses to boot. Unknown `ENV`, `LOG_LEVEL`
  (parsed once, into `Config.LogLevel`) or `CHAT_EFFORT` (the API's
  `reasoning_effort` set, `shared.ReasoningEffort`); a
  `DJANGO_BASE_URL` or `CHAT_BASE_URL` that is not an absolute http(s)
  URL with a host and no user info, query or fragment; an image URL
  template without `{assets_host}` and `{path}` or with another
  placeholder than those and `{schema}`; an `INTERNAL_EVENTS_SECRET`
  that is not a valid header value (control characters, leading or
  trailing blanks) or, in production and staging, shorter than
  `minInternalSecretLen`; `FEED_STALE_TTL` shorter than
  `FEED_FRESH_TTL`; any numeric setting at zero or below. `ENV`
  (production | staging | development | test) is folded into
  `AllowLocalWebhooks`, `PaymentHandlerEnv` and the secret's minimum
  length there so no handler compares environment strings.
  The infra staging overlay sets `ENV=staging`: production network
  rules, sandbox handler label. `STOREFRONT_DEFAULT_LOCALE` (required, a
  bare language code) is the storefront build's `DEFAULT_LOCALE`
  (`i18n/locales.ts`): the language its unprefixed pages render on
  every tenant, whatever the tenant's own default.
  Per-tenant values come from Django `tenant/resolve` at request time,
  never env.
- `internal/identity` — optional-auth middleware on /mcp: a present
  `Authorization: Bearer` is verified against Django `/agent/me`
  (60s in-memory cache, SHA-256 keys); invalid tokens get 401 + the
  RFC 9728 `WWW-Authenticate` challenge; anonymous requests pass
  through, except a call to an account tool, which gets the same
  challenge naming the scopes of every account tool the store serves
  (`mcpsrv.RequireLinkedAccount`), because a client starts OAuth on a
  401 and on nothing else. Account tools forward the bearer — Django
  enforces scopes.
- `internal/adminmcp` — `/mcp/admin`, a separate MCP server with its own
  tools and middleware. `RequireStaff` refuses a call without a staff
  bearer (401 + RFC 9728 challenge naming every read scope and
  `/.well-known/oauth-protected-resource/mcp/admin`), verifies one against
  Django `/agent/staff/me` (60s cache keyed by SHA-256(schema, token)) and
  forwards it to `/api/v1/agent/staff/*`, which checks the scope, the role
  and the team on every call. A scope not granted or not open to the role
  is refused before Django is asked; a 403 from Django is reported as
  `not_allowed` here (the person needs to know it is their role), unlike
  `/mcp`, and a 404 is checked against `/agent/staff/me` first, so a store
  that switched access off answers `access_off`, never "no data".
  Customer-written text comes back as `{untrusted: true, source, text}`. Staff tokens come from Django's staff
  issuer, never the store's own (that one signs in customers), and are
  bound to the store's `/mcp/admin` on its primary domain. Every tool is
  read only; a write tool needs the owner's approval first
  (`grooveshop-docs/src/dev/flows/staff-mcp.md`).
- `internal/tenant` — resolver (process memory 5m → Redis `ag:tenant:<version>:{host}`
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
  to; a 401 there is `ErrCartTokenRefused`. The read-only shipping calls
  (`/shipping/options`, BOX NOW `lockers/nearest`) send it the same way
  (`shippingCartHeaders`), so a chat shopper's cart is judged for parcel
  fit. Stock, order and payment calls never carry it.
  A call made while serving a request names that request's caller, as
  the storefront's SSR proxy does (`django.RelayCaller`, mounted in
  `server.New`): `CF-Connecting-IP` as `X-Real-IP`, `X-Forwarded-For`,
  `X-Origin-Verify` and `User-Agent`, relayed as received. So Django's
  throttles and `Idempotency-Key` scopes see the visitor, not the pod.
  The gateway decides and mints nothing here: Django believes the
  address only beside a correct `X-Origin-Verify` (`core/client_ip.py`),
  and the inbound `X-Real-Ip` (Traefik's own peer) is never relayed.
  Every call made with a request's context (`Client.do`) also sends its
  request id as `X-Correlation-ID`, the id Django logs the call under
  (`core/middleware/correlation_id.py`).
- `internal/httpmw` — request-id, access log, rate limit, RED metrics,
  recover, outermost first (`server.withGlobalChain`). Recover wraps the
  mux directly, so a handler panic is logged and still gets its access
  line and metric: a 500, or the status already sent if the response
  had begun (the status writer keeps the first one). A panic in the
  middleware itself is outside it: net/http recovers it, drops the
  connection and logs it at error through the gateway's logger
  (`Server.ErrorLog` in `serve.go`), with no access line or metric.
  `http.ErrAbortHandler` is passed on to net/http, which aborts the
  response silently. Metrics sits right around Recover and sees the
  `r.Pattern` the mux sets. An inbound `X-Request-Id` keeps its visible
  ASCII only, trimmed and cut to 64 bytes (the 64 characters Django keeps
  of `X-Correlation-ID`), else one is minted.
- `internal/obs` — slog JSON to stdout, Prometheus registry, health.
  The logger's context handler adds `request_id` and `tenant` to every
  line logged with a request's context (`obs.WithRequestID`; the tenant
  slot the access log installs and the tenant middleware fills with
  `obs.SetTenant`, as do the `/internal/*` routes from their body). A
  context detached with `context.WithoutCancel` keeps both, so work
  that outlives its caller and `obs.Recover` still name the request. Log
  with the context whenever one is in scope (sloglint enforces it); a
  line naming a store uses the key `tenant` (`obs.KeyTenant`), never
  `schema`, and only where no request context carries it.
  `/healthz` is process-only; `/readyz` gates on Redis ONLY — Django-down
  must never flip readiness (agents get actionable errors, not 502s).
- `internal/server` — mux assembly; new route groups register here.
- `internal/storefront` — every link into the Nuxt storefront (product,
  cart claim, order success, legal pages, OAuth metadata). The paths are
  a cross-repo contract with the Nuxt `app/pages` and `server/routes`
  trees; build URLs here, never with a `"https://" + domain + path`
  literal in a renderer. Page links are in a language: `storefront.Site`
  (built once from `STOREFRONT_DEFAULT_LOCALE`) hands out
  `Links(domain, lang)`, which leave the build default unprefixed and
  put any other language under `/<code>` (@nuxtjs/i18n
  `prefix_except_default`). The cart claim is a Nitro server route, not
  a page, so it takes no prefix: it carries a `locale=<code>` query instead,
  only for a language other than the build default, and the storefront
  redirects to the cart in it. A tool builds its links in the turn's
  language (`mcpsrv` `linksFor`: `django.Language`), so a chat turn on
  `/en` links `/en/...` and `/mcp` links the store's default; ACP, the
  UCP order webhooks, the Server Card and the feeds link the store's
  default.
- `internal/feeds` — one catalog sweep renders every feed, cached in
  Redis (`ag:{schema}:feed:{format}` plus `:meta`) between
  `FEED_FRESH_TTL` and `FEED_STALE_TTL`. A failed generation is logged
  once with the request that started it, counted
  (`gateway_feed_generation_failures_total`) and marked
  (`ag:{schema}:feed:failed`, `failureBackoff`): until the mark expires
  or an invalidation drops it, a stale feed is served without a refresh
  and a missing one answers 503 without a sweep; a failure an
  invalidation overtook is not marked. A revalidation reads the meta key
  and only checks the blob exists (Redis evicts keys one by one): an
  entry without its blob is a miss and regenerates. The sweep is
  detached from the request (`context.WithoutCancel`) and names no
  caller to Django (`django.WithoutCaller`): it is the store's work, not
  the crawler's. Field limits are the strictest of Google, Meta and
  TikTok, named at the top of `rss.go`. `internal/httphdr` parses
  `If-None-Match` and `Accept-Encoding` for the feeds and the Server
  Card alike.
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
  The other failures an agent acts on by their kind are `ToolError`s
  too: no linked account (`unauthorized`), a token without the tool's
  scope (`not_granted`, as on `/mcp/admin`), a refused coupon (its
  reason as the code) and the checkout store down
  (`errCheckoutUnavailable`, one value, `retryAfterSeconds` as an
  upstream outage). A checkout write that failed after another landed
  says so on top of its cause's hint (`withHint`). A list with nothing
  in it is `[]`, never `null` (`TestToolOutputsListNothingAsEmptyArrays`).
  `find_pickup_points` reads Django's 400 from BOX NOW's nearest lookup
  as no locker found: Django answers every BOX NOW refusal with it.
  Returning a plain Go error from a typed tool handler is fine — the
  SDK wraps it into exactly that; only a `*jsonrpc.Error` becomes a
  protocol error, so never return one for a business condition. The single exception is UCP discovery: a platform
  profile that cannot be fetched, validated or version-matched is the
  spec's transport error `-32001`, and a spent discovery budget is its
  retryable `-32000` (`mcpsrv.negotiate`).
- **ACP errors are the spec's** (`internal/acp/handler.go`): every
  error written by `internal/acp` is checked against the vendored
  `Error` schema, which requires `type`, `code` and `message`. Answers
  the middleware writes before the handler (tenant, rate limit,
  recover, the mux's own 404/405) are not.
  - A request whose `API-Version` is not `acp.Version` gets 400
    `missing_api_version` or `unsupported_api_version` with
    `supported_versions`.
  - 409 is only `idempotency_in_flight`, with `Retry-After`: a key or
    the session lock in use, or an order being placed until
    `Flow.CompletionDeadline`. Past it the outcome was never recorded
    and stays so until the session expires: 502
    `order_outcome_unknown`, no `Retry-After`.
  - A completed or canceled session refuses update and cancel with
    405 and an `Allow` header, and so does a canceled one complete. A
    complete of a completed session answers 200 with the session.
  - A create stores the response it sent (status and body) with its
    key (`createScope`, versioned with that record); a replay with the
    same body answers those bytes for the record's 24 hours, whether
    or not the session lives. Update, complete and cancel replays
    render the session as it is now. `Idempotent-Replayed` is set only
    on a replay's own answer.
  - So the buyer data a create carried (name, email, phone, address)
    is kept in Redis for 24 hours under
    `ag:{schema}:acp:idem:create.v2:{key}`, while the session itself
    lives 30 minutes: the spec requires a key's response to be kept at
    least 24 hours (§6.6). Nothing else reads it.
  - A create is rendered before it is saved, and a 4xx or 5xx releases
    its key: a create that failed leaves no session and no record.
  - `completeError` answers every `checkout` and `django` sentinel
    deliberately (`TestCompleteErrorMapsEverySentinel` walks them): a
    refusal is a 4xx, and an order that exists or may is never
    answered as a failure.
- **A completion finishes without its caller.** `checkout.Flow.Complete`
  runs on the caller's context only until the order is about to be
  created. From the in-progress save on it is detached
  (`context.WithoutCancel`) and bounded by three of the Django
  client's timeouts (`completionSteps`), because Django has the order
  whether or not the caller's context is still live. That context ends
  when the platform disconnects (an ACP request's always; an MCP
  tool's for a client on protocol 2026-07-28 or later,
  `PropagateRequestCancellation`) and when the server cancels its
  requests at the end of the shutdown drain (`serve.go`): the same
  three checks cover both. A call whose context has ended before the
  detach places no order; after a placed order it is answered "order
  placed, read it again" without the upstream read, with an info log
  line. The process exiting still cuts a completion: the drain is
  shorter than the budget (issue #117). `Flow.Complete` alone stores
  the session and the order link from the in-progress save on, and
  returns a `checkout.Placement`: `OrderMayExist` (the zero value)
  spends the caller's idempotency key, `NothingPlaced` releases it. A
  completion refused before the in-progress save stores nothing. So
  the ACP handler and the UCP tool store what the call itself
  submitted (ACP's buyer, UCP's payment) before they call it, so that
  a refused call keeps the buyer's latest choice, and only for a
  session that is still open (`Session.Open`). ACP stores it under the
  session lock and its idempotency key, before any Django call. UCP
  stores it under its completion claim: its payment lookup can
  outlast the lock, which may have lapsed by then (#84). After the
  flow they only settle their own key by the placement (ACP's
  body-hash record, UCP's completion claim), on a context the caller
  cannot cancel, bounded by the Django client's timeout, and map the
  error; neither saves the session again.
- **UCP** (negotiation, the canonical checkout tools' wire shapes, the
  OpenRPC `schema` URL, `get_order`, order webhooks): the conventions are in
  `.claude/rules/ucp.md`, which loads with `internal/ucp/**`. Read it before
  touching UCP from anywhere else.
- **A store feature switched off is off on every surface.** Django
  folds each into one boolean, the plan flag AND the store setting, under
  `features` on `tenant/resolve` (`TenantConfig.Features`); read
  nothing else. A tool serving a feature is registered with `onlyWith`
  (`internal/mcpsrv/server.go`): the catalog tools need `catalogue`;
  the cart, shipping, payment-method, checkout-link and five canonical
  checkout tools need `cart` (which Django folds the catalogue into);
  `get_product_reviews`, `subscribe_product_alert` and `my_favourites`
  need `catalogue` and `productReviews` / `productAlerts` /
  `favourites`; `my_loyalty_points` `loyalty`. The order tools and
  `get_store_policies` stay open (return terms bind orders already
  placed). The UCP profile and `/acp/*` need `cart` (`CheckoutOn`), the
  feeds `catalogue` (`ProductFeedsOn`). The per-store server (cached per feature set) neither
  lists nor serves it, discovery does not advertise it, and the chat
  bridge, which shares one server built with `EveryFeature`, filters
  through `mcpsrv.Offered`. With reviews off no rating or review count
  leaves the catalog tools either (ratings are on Django's 1–10 scale).
  With promotions off `checkout.ApplyDiscountCodes` refuses every code
  as `discount_code_invalid` without applying it (Django answers 404
  with the plan flag off, which would fail the request) and clears the
  cart's old coupon best effort, and ACP sessions do not declare the
  discount extension.
- **Store facts come from the store, never a default.**
  `find_pickup_points` searches only the pickup carriers the store's
  `/shipping/options` list as `pickup_point` for the country and the
  cart (`FitsCart`), among the networks it can search (ACS, BOX NOW);
  a store without one gets an answer saying so, not an error. BOX NOW's
  `lockers/nearest` requires city, street AND postal code, so it is
  called only with all three and the tool tells the model to ask for
  the rest. `get_store_policies` reads `RETURN_WINDOW_DAYS` and
  `RETURN_FEES` from `/settings/public` (Django's `PUBLIC_SETTING_KEYS`;
  `0` / `""` are "not stated" and are left out, a missing or malformed
  value fails the call) and links `Links.ReturnPolicy` only while
  `/content-page/return-policy` is published (a 404 means it is not).
- Redis keys: `ag:{schema}:…`; tenant cache `ag:tenant:<version>:{host}`,
  versioned with the cached `TenantConfig` shape: bump `redisKeyVersion`
  (`internal/tenant/resolver.go`) when a field is added, or entries an
  older pod wrote decode it as its zero value.
  `TestCachedShapeMatchesKeyVersion` pins the shape (a golden in
  `internal/tenant/cacheguard_test.go`) to that version: a changed shape
  fails until the golden is updated, which prompts the bump, and the
  golden's diff shows a shape change in review. A field Django leaves out
  of its schema (`contract:"undocumented"`) is one it sends only to the
  gateway's authenticated call, and none of those is written to the shared
  Redis entry: `publicConfig` clears each, and
  `TestPublicConfigClearsEveryUndocumentedField` finds them by reflection.
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

`contract/django-consumer.openapi.json` is generated by
`go test ./internal/django -run TestDjangoConsumerContract -update` and is
the gateway's side of the infrastructure deploy gate. How it is built, read
and tagged: `.claude/rules/django-consumer-contract.md` (loads with
`internal/django/**` and `contract/**`).

A pull request that changes the contract also runs the API contract check
(`.github/workflows/api-contract.yml`, required on `main`): the contract
must not require anything the Django that production and staging run does
not serve. The rule and how to split a change are in
`grooveshop-django-api/api-contract/README.md`.

## Cross-repo contracts

- Django repo (`../grooveshop-django-api`): `tenant/resolve` endpoint shape,
  cart `X-Cart-Id` UUID header, guest checkout, `create_checkout_session`
  (Viva Smart Checkout URL). Payments: **Viva primary** (hosted
  authorization via UCP `requires_escalation`+`continue_url`) or a
  collect-later pay way (cash on delivery). Delegated card payments
  (Stripe or any tokenized instrument) are refused today: ACP
  `complete` answers 400 `unsupported` to any `payment_data` handler or
  instrument (`internal/acp/handler.go`), and UCP advertises and accepts
  only collect-later instruments (`resolvePayWay` in
  `internal/mcpsrv/ucp_canonical.go`).
- Nuxt repo: `/cart/claim` (query `uuid=<cart>`, plus
  `locale=<code>` for a language other than the build default) claims
  a gateway-built cart into the browser session and lands on the cart
  page in `locale` (absent: the build default);
  `.well-known/oauth-protected-resource[/mcp]` names the Django API as
  the OAuth authorization server (allauth.idp: auth-code + PKCE + DCR).
- Agent discovery is the gateway's alone: `/.well-known/ard.json` and
  `/.well-known/ai-catalog.json` are both cut from one entry
  (`mcpsrv.storeEntry`), so they cannot drift from each other or from
  the Server Card. `identifier` is `urn:air:<primary domain in
  ASCII>:mcp:store`, and the entry and the Server Card's endpoint name
  the primary domain too, because a client refuses to link an account on
  a host other than the resource the OAuth metadata names; `capabilities` come from the same `toolset` that
  `NewServer` registers for the store's features (`toolNamesFor`), so a
  new tool is advertised automatically and a switched-off one is not;
  `representativeQueries` sit beside the tool definitions. Both share
  `/mcp`'s agent-commerce gate (a store without it gets the same 404),
  and the Server Card's CORS, ETag and cache headers.
- Nuxt chat widget (`app/composables/useShopChat.ts`) reads the `/chat`
  SSE events; the request, events, locale, `turnId`, `cartToken`,
  `attribution` and `products` rules are in
  `.claude/rules/chat-sse-contract.md` (loads with `internal/chat/**`).
  A change there is a storefront change too.
- Cart source: every agent cart add sends `attribution.agentProtocol`,
  which Django believes beside the internal secret every cart call
  carries and books as "AI agent": UCP/ACP checkouts their protocol
  (`checkout.SyncCartLines`, like their orders), `/mcp`'s `add_to_cart`
  `mcp` (`django.AgentProtocolMCP`, `handlers.cartAttribution`). The
  chat's `add_to_cart` never sends a protocol: it sends the shopper's
  first-touch `attribution` the storefront relayed
  (`mcpsrv.WithShopperAttribution`), or none.
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

  Inside a turn (`chat.runTurn`):
  - At most `CHAT_MAX_ITERATIONS` model calls. The last one is sent `tool_choice: none`, and tool calls a provider returns there anyway are not run.
  - The model client is built once (`chat.New`) with no SDK retries. A provider throttle, answered as a 429 or sent as an error inside the stream (`chat.providerErrorOf`), ends the turn after one call with SSE `error {code: rate_limited, retryAfter}`.
  - `CHAT_TURN_TIMEOUT`, counted from the turn's start, bounds the model calls and when a tool call may start. Once it has passed, no further tool call of the batch starts, and the turn ends with the generic, retryable `error {message}`; a stalled provider ends the same way. A tool call already running is never cut, so a cart call is not cut midway. A turn can still fail after a cart change Django accepted, with nothing telling the shopper: that is #94. A retry with the same `turnId` does not add a product twice: `add_to_cart` sends an `Idempotency-Key` derived from the turn (`mcpsrv.Turn`), which Django's `core/middleware/idempotency.py` answers with the first outcome.
  - A shutdown cancels the deadline's context too, and `handle` checks for it (`httpmw.ErrShuttingDown`) before anything else, so a turn cut by shutdown is reported as one.
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

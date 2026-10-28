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

CI runs lint → unit+integration (with `-race`) → build. `-race` requires
cgo; the local Go builds with `CGO_ENABLED=0` and has no C compiler, so
`go test -race` fails at once with `-race requires cgo`. Run it in CI.

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
  through. Account tools forward the bearer — Django enforces scopes.
- `internal/tenant` — resolver (process memory 5m → Redis `ag:tenant:v4:{host}`
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
- Redis keys: `ag:{schema}:…`; tenant cache `ag:tenant:v4:{host}`,
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

`contract/django-consumer.openapi.json` is generated by
`go test ./internal/django -run TestDjangoConsumerContract -update` and is
the gateway's side of the infrastructure deploy gate. How it is built, read
and tagged: `.claude/rules/django-consumer-contract.md` (loads with
`internal/django/**` and `contract/**`).

## Cross-repo contracts

- Django repo (`../grooveshop-django-api`): `tenant/resolve` endpoint shape,
  cart `X-Cart-Id` UUID header, guest checkout, `create_checkout_session`
  (Viva Smart Checkout URL). Payments: **Viva primary** (hosted
  authorization via UCP `requires_escalation`+`continue_url`), Stripe
  tokenized behind per-tenant flag.
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
  ASCII>:mcp:store`; `capabilities` come from the same `toolset` that
  `NewServer` registers for the store's features (`toolNamesFor`), so a
  new tool is advertised automatically and a switched-off one is not;
  `representativeQueries` sit beside the tool definitions. Both share
  `/mcp`'s agent-commerce gate (a store without it gets the same 404),
  and the Server Card's CORS, ETag and cache headers.
- Nuxt chat widget (`app/composables/useShopChat.ts`) reads the `/chat`
  SSE events; the request, events, locale, `turnId`, `cartToken` and
  `products` rules are in `.claude/rules/chat-sse-contract.md` (loads with
  `internal/chat/**`). A change there is a storefront change too.
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

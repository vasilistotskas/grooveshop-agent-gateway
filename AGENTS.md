# AGENTS.md

Guidance for AI coding agents (Claude Code, Codex, Antigravity, OpenCode) working in this repository.

## Project Overview

GrooveShop **agent gateway** — a Go microservice that makes every tenant
storefront AI-shoppable. One binary serves six public surfaces on the
storefront's own domain (Traefik path-routes them here): the shoppers'
MCP server, the store-admin MCP server, UCP, ACP, the product feeds and
the chat, with the documents that describe them, plus cluster-only
internal endpoints and the operational routes:

| Path | Surface |
|---|---|
| `POST /mcp` | MCP server (stateless streamable HTTP): the UCP checkout capability's five canonical tools + `get_order` + ergonomic commerce tools + account tools (`my_orders`, `my_loyalty_points`, `my_favourites`); a tool whose store feature is off is neither listed nor callable. A platform's RFC 9421 signature is verified on any request that carries one, and `get_order` takes only signed ones (`mcpsrv.VerifyPlatformSignature`) |
| `POST /mcp/admin` | Store-admin MCP server (`internal/adminmcp`): the store's own team reads orders, sales, profit, products and stock, customers, reviews and contact messages, by role, read only. Every call needs a staff bearer from Django's staff issuer on `STAFF_CONNECT_HOST`; `GET /.well-known/oauth-protected-resource/mcp/admin` names it |
| `GET /mcp/server-card` | MCP Server Card (SEP-2127): identity and remotes only — no tools, which clients read from `tools/list`. Name/title/version match `initialize` (`mcpsrv.IdentityFor`) |
| `GET /.well-known/ard.json` | Agentic Resource Discovery manifest (ARD v0.91, `application/json`): one entry for the MCP server, with the card's title/description, the tool names the store's `tools/list` serves as `capabilities` and `representativeQueries` |
| `GET /.well-known/ai-catalog.json` | AI Catalog (`application/ai-catalog+json`): the same entry as `ard.json`, name and description included, because Lighthouse validates this document against the ARD schema |
| `GET /.well-known/ucp` | UCP business profile (spec 2026-08-25), incl. the `space.grooveshop.payments` handler |
| `/acp/*` | ACP agentic checkout REST (spec 2026-04-17) |
| `/feeds/*` | Product feeds: google.xml, meta.xml, tiktok.xml, acp.json |
| `POST /chat` | First-party shopping chatbot (SSE; OpenAI-compatible protocol via `internal/ai` — provider, model, API key and reasoning effort per store from Django's `tenant/resolve`) |
| `/internal/*` | Cluster-only, shared-secret header: Django's order events, sent by its outbox (`POST /internal/events/order-status`) and feed invalidation (`POST /internal/feeds/invalidate`) |
| `GET /healthz`, `GET /readyz` | Liveness / readiness (readiness gates on Redis and the startup check of the signing keys; Django is reported, never gating) |
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
go tool govulncheck ./...                        # Known vulnerabilities the code reaches
go mod tidy -diff                                # go.mod and go.sum are tidy
go run ./cmd/gateway                             # Run (needs .env vars exported)
docker build -t agent-gateway .                  # Production image
```

CI (`ci.yml`) runs three jobs in parallel, on purpose: lint (with the
vulnerability and tidy checks), the unit and integration tests (with
`-race`) and the build. `-race` requires
cgo; the local Go builds with `CGO_ENABLED=0` and has no C compiler, so
`go test -race` fails at once with `-race requires cgo`. Run it in CI.

The `/gateway-test` skill wraps these. `.claude/` also registers a
`gateway-contract-reviewer` subagent for the invariants below and in
`.claude/rules/`, and hooks that
`gofmt` and `go vet` each edited file.

The real-stack test (`internal/realstack`) runs the gateway image against a
real Django, its database and its search, including a real sign-in. It takes
about 12 minutes, so `.github/workflows/real-stack.yml` runs it before a release,
nightly and when the test changes. Locally it needs `REALSTACK_DJANGO_IMAGE`
and `REALSTACK_GATEWAY_IMAGE`, and a lot of memory (`/gateway-test
realstack`).

## Releasing

A release is a pushed `vX.Y.Z` tag. Before tagging, get an independent review of
everything since the last tag (`git diff <last tag> origin/main`), briefed with
the code and not with your conclusions, and read it before you tag. CodeRabbit
and PR Lens read one pull request at a time and have passed bugs a full read
found. A test written with its fix does not prove the contract: check the other
side's code. `release.yml` also refuses to publish unless the real-stack test
passes, and runs CI's tidy, image and vulnerability checks again.

## Architecture

Each row of this map imports only rows below it, and within a row only
along its arrows (`go list -f '{{.Imports}}'` shows it). Each package's
detail is in the rule that loads with its files.

```text
cmd/gateway    cmd/ucp-sign                  wiring and serve; sign as a platform
internal/server                              mux assembly, /internal/* routes
chat → mcpsrv, ai    adminmcp    acp    feeds    the surfaces
ucp                                          profile, negotiation, webhooks, keys
checkout    identity → mcptool               sessions; bearers; tool answers
tenant                                       resolver and middleware
django                                       the typed Django client
config  httpmw → obs  httphdr  httpsig  media  money  storefront  text
```

| Package | What it is | Rule |
|---|---|---|
| `cmd/gateway` | Wiring: config → clients → mux, then `serve` (`serve.go`), the one piece of logic, tested; the shutdown cut and its final window | `shutdown.md` |
| `cmd/ucp-sign` | Signs a request to a running gateway as a UCP platform | `ucp.md` |
| `internal/config` | Env config, fail-fast `Load()`: a value that would boot and then misbehave refuses to boot. Per-tenant values come from Django `tenant/resolve` at request time, never env | `config.md` |
| `internal/server` | Mux assembly; new route groups register here | `httpmw-obs.md`, `acp.md` |
| `internal/mcpsrv` | `/mcp`: commerce, UCP and account tools, discovery, the Server Card, the platform signature check; the chat's server | `mcp-tools.md`, `store-features.md`, `ucp.md`, `checkout.md` |
| `internal/adminmcp` | `/mcp/admin`, the store's own team, read only | `adminmcp.md` |
| `internal/ai` | Shared OpenAI-compatible model client; calls each store's own provider, model and API key from `TenantConfig.AIProfile` | |
| `internal/chat` | `/chat`, the storefront's chatbot (SSE) | `chat-sse-contract.md` |
| `internal/acp` | `/acp/*` | `acp.md`, `checkout.md` |
| `internal/ucp` | UCP profile, negotiation, order link, webhooks, signing keys | `ucp.md` |
| `internal/httpsig` | RFC 9421 message signatures with RFC 9530 `Content-Digest` | `ucp.md` |
| `internal/checkout` | Checkout sessions, their completion and the cart each holds | `checkout.md` |
| `internal/feeds` | Product feeds, cached in Redis | `feeds.md` |
| `internal/identity` | The bearer verification both MCP servers share | `identity.md` |
| `internal/mcptool` | What the tools of all three MCP servers answer with, and `Add` | `mcp-tools.md` |
| `internal/tenant` | Resolver (memory → Redis → Django) and middleware, from the real `Host` only | `tenant.md` |
| `internal/django` | Typed Django API client | `django-consumer-contract.md` |
| `internal/httpmw`, `internal/obs` | Request id, access log, rate limit, metrics, recover; slog JSON, Prometheus, health | `httpmw-obs.md` |
| `internal/storefront` | Every link into the Nuxt storefront | `storefront-links.md` |
| `internal/httphdr` | `If-None-Match` and `Accept-Encoding` parsing for the feeds and the Server Card | `feeds.md` |
| `internal/text` | Rune-safe truncation. Byte-index cuts split Greek runes; use `text.Runes` / `text.Ellipsize` | |
| `internal/media`, `internal/money` | Image URLs from the store's template; integer cents | |
| `internal/integration`, `internal/realstack` | Docker-backed tests; the gateway image against a real Django | |
| `testdata/fixtures/django/` | Recorded upstream responses for the DTO decode tests. They are hand-recorded, so they prove decoding, not that Django still sends a field: that is the consumer contract's job (below) | `django-consumer-contract.md` |

Rules live in `.claude/rules/` and load when a file their `paths:` names
is read; read the one for an area before changing it from elsewhere.

## Conventions

- Hand-written minimal DTOs (camelCase tags); decode with `UseNumber()`,
  money as `json.Number` → integer cents internally, format at the edge.
- **A tool's failure is a `CallToolResult{IsError: true}` with actionable
  text**, a `mcptool.ToolError` rendered as JSON; never a
  `*jsonrpc.Error` for a business condition (UCP discovery is the one
  exception). The codes, the cart and checkout refusals and each
  server's `upstreamErr`: `.claude/rules/mcp-tools.md`.
- **ACP errors are the spec's**, every one checked against the vendored
  `Error` schema, those answered before the handler included:
  `.claude/rules/acp.md`.
- **Checkout sessions** (`.claude/rules/checkout.md`): a checkout no
  session is stored for is not "nothing happened"; a session answers
  only on the surface, and to the platform, that created it; a
  completion finishes without its caller; one completion places a
  session's order, and no guard that expires decides which; an order
  Django left unanswered is sent again with the checkout's id, which
  Django keeps one order per, and the next complete resumes one still
  unanswered; no answer
  hands out a new checkout while an order for the old one may exist; one
  live checkout holds a cart; its totals are Django's checkout quote,
  and its order is placed only at the total it showed, from the cart it
  showed.
- **UCP** (negotiation, the canonical checkout tools' wire shapes, the
  OpenRPC `schema` URL, `get_order`, order webhooks): the conventions are in
  `.claude/rules/ucp.md`, which loads with `internal/ucp/**`. Read it before
  touching UCP from anywhere else.
- **A store feature switched off is off on every surface.** Django folds
  each into one boolean under `features` on `tenant/resolve`
  (`TenantConfig.Features`); read nothing else. **Store facts come from
  the store, never a default.** Both: `.claude/rules/store-features.md`.
- **Logging.** Log with the request's context whenever one is in scope
  (sloglint enforces it), so the line carries `request_id` and `tenant`;
  a line naming a store uses the key `tenant` (`obs.KeyTenant`), never
  `schema`. `/readyz` never gates on Django
  (`.claude/rules/httpmw-obs.md`).
- **Links into the storefront** are built in `internal/storefront`, never
  with a `"https://" + domain + path` literal
  (`.claude/rules/storefront-links.md`).
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
  A checkout session is `ag:{schema}:cs:v8:{id}`, its lock that plus
  `:lock`, the order it placed that plus `:order`. The version is of how sessions are written, not only of their
  shape: bump it whenever an older pod writing the key its own way could
  undo what a newer one relies on (v1 pods wrote sessions unconditionally;
  v2 pods saved one without keeping its cart's record alive; v3 pods
  know nothing of the platform that signed a session's creation, and
  their save drops it; v4 pods know nothing of the expiry a session
  published, and their save drops it; v5 pods know nothing of the total
  a session showed, which its order is placed at; v6 pods send an order
  without its checkout's id, so a resume of their session would order
  again; v7 pods know nothing of the cart digest a session showed, a
  delivery's address line 2 or a pickup point without an address, and
  their save drops them),
  so the two never share a session; a session under an older key is not
  found. `TestSessionKeyIsVersioned` pins it. The session that holds a
  cart is named in `ag:{schema}:checkoutcart:{cartId}` (`cartKey`), a
  family of its own: every key under `cs:` belongs to one session.
  A store's UCP signing key is `ag:{schema}:ucp:signing_key:es256:aes256gcm`,
  sealed (see `.claude/rules/ucp.md`): no Redis key holds a secret in
  clear.
  The gateway's Redis is its own instance, `redis-gateway` (database 0),
  shared with no other service, with `maxmemory-policy noeviction` and AOF persistence (the
  infrastructure repository's manifests): a record without a TTL (the
  signing keys) or with a long one (the order links, a year) is never
  evicted. A full Redis refuses writes instead of dropping records.
  What the code does for a record that is gone (a session's cart record
  written again, a missing order link) stays: a flush or a restore can
  still lose one.
  A Redis write that depends
  on what the key holds is a script (`redis.NewScript`), never a read
  followed by a write.
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

## How to add

Each step names the guard that fails when it is skipped.

- **A tool gated on a store feature** (`internal/mcpsrv/server.go`): a
  `tool` in the toolset, wrapped in `onlyWith(<feature>On, …)`, with its
  surface trait (`chatOnly`, `mcpOnly`, `account`) and an annotation from
  `mcptool` (`ReadOnly`, `Adds`, `Replaces`, `Changes`); `mcptool.Add`
  registers it, and its output lists nothing as `[]`. Guards:
  `TestEveryToolIsAnnotatedAndTheListCacheable`,
  `TestStoreIsServedOnlyTheToolsOfItsFeatures`,
  `TestToolOutputsListNothingAsEmptyArrays`,
  `TestARDCapabilitiesAreTheRegisteredTools` (discovery advertises it by
  itself), `TestChatServerListsNoToolThatNeedsMeta`.
  `.claude/rules/store-features.md`, `.claude/rules/mcp-tools.md`.
- **A Django call** (`internal/django`): a `Client` method through
  `c.get` or `c.send`, a minimal camelCase DTO decoding money as
  `json.Number`, a recorded response under `testdata/fixtures/django/`
  with its decode test, the call in the contract's registry, and the
  contract regenerated (`go test ./internal/django -run
  TestDjangoConsumerContract -update`). Guards:
  `TestDjangoContractCoversEveryCall`, `TestDjangoConsumerContract`, the
  API contract workflow. `.claude/rules/django-consumer-contract.md`.
- **A per-tenant config field** (`django.TenantConfig`, from
  `tenant/resolve`, never env): the field and its fixture, the contract
  regenerated, then `redisKeyVersion` bumped with the golden in
  `internal/tenant/cacheguard_test.go`. A credential Django sends only to
  the gateway's authenticated call is `contract:"undocumented"` and is
  cleared from the shared Redis entry. Guards:
  `TestCachedShapeMatchesKeyVersion`,
  `TestPublicConfigClearsEveryUndocumentedField`. `.claude/rules/tenant.md`.
- **A staff tool** (`internal/adminmcp/server.go`): `mcptool.Add` with
  `mcptool.ReadOnly`, called through `handlers.call` with its read scope
  (refused before Django is asked), against a route under
  `/api/v1/agent/staff/*`; customer-written text comes back
  `mcptool.Untrusted`. Only read tools: a write tool needs the owner's
  approval first. Guards: `TestEveryToolOnlyReads`,
  `TestAScopeNotGrantedIsRefusedWithoutAskingTheStore`,
  `TestCustomerTextComesBackMarkedUntrusted`, the admin
  `TestToolOutputsListNothingAsEmptyArrays`. `.claude/rules/adminmcp.md`.

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
  arrives as `acpBearerToken` and the store's AI provider, model, base URL,
  key and reasoning effort as `aiProfile` on `tenant/resolve` — Django includes both only
  when the resolve call carries the gateway's `X-Internal-Token` (same
  shared secret as the order-event push). `/chat` sits behind the
  agent-commerce gate (`tenant.RequireAgentCommerce`), whose 404 is the
  generic one; a store with agent commerce on and no AI profile gets a
  localized 404 from the chat handler. A tenant without an ACP token has
  ACP disabled — every bearer gets 401 from `/acp/*`. `/mcp/admin` is
  not behind the agent-commerce gate: its own gate is the store's
  `staffAiAccess` feature (`.claude/rules/adminmcp.md`).
- Chat spend is bounded in three layers (Traefik, the gateway's
  per-(host, client) limiter and a Redis turn quota), and a turn by its
  own limits: `.claude/rules/chat-sse-contract.md`.
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

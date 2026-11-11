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
- `internal/identity` — the bearer verification both MCP servers share,
  and /mcp's optional-auth middleware.
  - `Verifier[P]` caches a `Probe`'s verdict for 60s, keyed by
    SHA-256(schema, token), so a verdict for one store never answers for
    another. The cache holds at most 4096 entries and is flushed when
    full. A valid token is cached, and so is Django's 401 (invalid).
    Anything else is returned uncached.
  - One implementation, one instance per server, never shared. The key
    names no server, so a shared instance would let a shopper's verdict
    stand for a staff one. The types keep them apart:
    `Verifier[*django.AgentProfile]` for /mcp and
    `Verifier[*django.StaffProfile]` for /mcp/admin.
  - Concurrent calls with one uncached token share one probe
    (singleflight). A caller whose context has already ended starts
    none. The probe is detached from its callers, as the tenant
    resolver's is, so it runs on when its last waiter leaves and its
    verdict is cached. It is bounded by the Django client: at most three
    attempts of `UPSTREAM_TIMEOUT` each, so it ends within
    3 × `UPSTREAM_TIMEOUT` + 0.5 s.
  - `Refuse` is the one answer, on both servers, to a bearer that did not
    verify:
    - invalid: a 401 and the RFC 9728 `WWW-Authenticate` challenge
      (`WriteChallenge`, naming the server's own metadata);
    - Django's 404: a 404. The store switched the surface off and this
      pod's copy has not caught up; it is the answer the gateway's own
      gate gives once it has;
    - Django's 429: a 429 with its `Retry-After`;
    - a caller that hung up: a 503 with no error line;
    - anything else: a 503, logged with `server` (mcp or mcp_admin).
  - On /mcp a present `Authorization: Bearer` is verified against Django
    `/agent/me` (`NewShopperVerifier`; a 403 there is a valid token
    without the `profile` scope).
  - Anonymous requests pass through, except a call to an account tool.
    That gets the challenge naming the scopes of every account tool the
    store serves (`mcpsrv.RequireLinkedAccount`), because a client starts
    OAuth on a 401 and on nothing else. The middleware reads exactly the
    bodies the MCP SDK's handler reads, with the SDK's bounds
    (`calledTools`): the SDK's nesting bound before any decoding
    (`sdkMaxDepth`, mirroring go-sdk's `internal/json`), a batch,
    case-sensitive keys, the first JSON value only. A body nested deeper
    is passed on undecoded and the SDK refuses it whole. It challenges
    when any message calls an account tool.
  - Account tools forward the bearer — Django enforces scopes. Each
    carries its scope on its definition (`account` in
    `internal/mcpsrv/server.go`), the challenge reads them off the toolset
    (`accountScopes`), and they are /mcp's alone: the chat server does
    not register them.
- `internal/mcptool` — what the tools of both MCP servers answer with:
  `ToolError` and every code the gateway mints, the `Untrusted` marker
  (`MarkUntrusted`) for text a customer wrote, and `Add`, through which
  every tool on /mcp, the chat and /mcp/admin is registered.
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
  - A handler panic is recovered there (see `internal/httpmw`).
  - Every tool has a `Title` and annotations (`mcptool.ReadOnly`, `Adds`,
    `Changes`), none open-world. `mcptool.Cacheable` lets a client keep
    `tools/list` for `TENANT_CACHE_TTL`: public on /mcp, private on
    /mcp/admin.

  How a Django failure maps to a code is each server's own `upstreamErr`.
- `internal/adminmcp` — `/mcp/admin`, a separate MCP server with its own
  tools and middleware. `RequireStaff` refuses a call without a staff
  bearer (401 + RFC 9728 challenge naming every read scope and
  `/.well-known/oauth-protected-resource/mcp/admin`), verifies one against
  Django `/agent/staff/me` (`identity.Verifier`, refused through
  `identity.Refuse`) and
  forwards it to `/api/v1/agent/staff/*`, which checks the scope, the role
  and the team on every call. A scope not granted or not open to the role
  is refused before Django is asked; a 403 from Django is reported as
  `not_allowed` here (the person needs to know it is their role), unlike
  `/mcp`, and a 404 is checked against `/agent/staff/me` first, so a store
  that switched access off answers `access_off`, never "no data".
  Customer-written text comes back as `{untrusted: true, source, text}`
  (`mcptool.Untrusted`). Staff tokens come from Django's staff
  issuer, never the store's own (that one signs in customers), and are
  bound to the store's `/mcp/admin` on its primary domain. Every tool is
  read only; a write tool needs the owner's approval first
  (`grooveshop-docs/src/dev/flows/staff-mcp.md`).
- `internal/tenant` — resolver (process memory 5m → Redis `ag:tenant:<version>:{host}`
  → Django; singleflight; 60s negative cache; stale-if-error) + middleware
  (tenant from the **real Host header only** — inbound X-Forwarded-Host is
  deliberately ignored; spoofing it must never switch tenants). A store
  it cannot resolve is a 503 with an error line, unless the caller hung
  up (`context.Canceled`), which logs nothing.
  Credentials (`chatApiKey`, `acpBearerToken`) live in process memory
  only: Redis holds the public config, so `/chat` and `/acp/*`
  (`MiddlewareWithSecrets`) top a Redis-tier tenant up with
  `EnsureSecrets`, which asks Django through the resolve's own path
  (`fromDjango`), so a top-up counts its answer on
  `gateway_tenant_cache_total` (each lookup counts the answer it serves
  once, where it returns it; a failure is not counted) and rewrites the
  public Redis entry as a resolve does; a store Django
  answers 404 for is a 404 there too and is cached as the negative entry
  in memory and Redis.
  - **Held credentials.** A memory entry keeps the last tenant this pod
    got from Django (`memEntry.held`) through a Redis hit. They belong
    to a store, not a host: `storeLocked` keeps them only while the
    tenant known for the host has the same `SchemaName`, and drops them
    for a gone store or a host that moved to another store.
  - **When Django cannot give the credentials** (down, or the token not
    recognised), the answer for credential callers is decided once, by
    `rememberLocked`, in the same lock section as the call that found
    out: the resolve's (`serveStale`, or an unrecognised answer in
    `fromDjango`) or the top-up's (`answerSecrets`).
    - The answer is the entry's current config with the held
      credentials (`withCredentials`), marked `Stale`, until two
      `TENANT_CACHE_TTL`s after the pod fetched them (`secretsUntil`);
      otherwise it is the failure, wrapped in `errNoCredentials`, and
      the route answers 503.
    - It is remembered on the entry (`memEntry.topUp`) for the negative
      TTL, capped at the bound. It is logged once, by the first
      credential caller that receives it (`receiveLocked`, `logTopUp`),
      never by a plain resolve: a Django failure on a route that needs
      no credential logs only the stale config's warning. The
      middleware does not log a remembered failure again. The result is
      one Django call and at most one error line per decision. A resolve
      and a top-up each decide at most once per host per pod per negative
      TTL, and a resolve that meets the outage while a top-up's answer
      is remembered decides again.
    - It never replaces the entry's tenant, which stays the freshest
      config Django, Redis or the stale-if-error path gave. The other
      routes serve that config, without credentials. The held answer
      carries the config the entry had when the answer was decided, so
      its features gate the credential routes as they gate the others
      then; a Redis refill that replaces the entry's config meanwhile
      reaches the credential routes only when the remembered answer
      ends, within one negative TTL.
    - A fresh recognised answer ends what was remembered.
    - `EnsureSecrets` serves credentials only for the store the
      caller's request resolved (`errAnotherStore`), so a caller that
      joined a flight decided before the host moved gets nothing of the
      old store.
  - **Rotation.** An ACP bearer rotated in Django is refused by a pod
    within one TTL while Django answers, and within two while it cannot;
    the chat key the same.
  - **Unrecognised token.** A resolve Django answered without
    recognising the gateway's internal token (a rotation that reached
    one side) is logged once at error with its cause ("Django did not
    recognise the gateway's internal token") and cached as a public
    config, in memory and Redis, never
    as a store without credentials. The plain routes serve it; the
    credential routes take the failed top-up's path above. Once Django
    recognises the token, credentials load within one negative TTL. A
    resolve that meets it counts a `miss`, like any Django answer; a
    top-up counts the held answer it serves as `stale`.
  - **Stale config.** A stale config carries no credentials and still
    has no age bound. A stale answer never replaces an entry another
    flight made live meanwhile.
- `internal/django` — typed API client. `ResolveTenant` also reports
  whether Django recognised the internal token: Django adds `chatApiKey`
  and `acpBearerToken`, possibly empty, only then, so their presence is
  the answer, decoded beside `TenantConfig` (`tenantResolve`), never in
  it; one key without the other is a decode error. Every tenant-scoped
  call sends
  `X-Forwarded-Host: <tenant domain>` + `X-Forwarded-Proto: https` (+
  `X-Language`). A GET is retried twice on a 5xx or a lost connection
  (refused, reset, closed, before the headers or in the body); never on a
  call cut by `UPSTREAM_TIMEOUT` (a hung Django would get it three
  times), never once its context has ended. Mutations, the `/readyz`
  ping and `GetCart` naming no cart (Django creates a guest cart for it)
  are sent once. "Once" is the client's loop: net/http itself re-sends a
  GET, or a request carrying `Idempotency-Key`, when a reused keep-alive
  connection dies before the first response byte. So `Idempotency-Key`
  goes only on a call Django deduplicates by it
  (`core/middleware/idempotency.py`): today the chat's `add_to_cart`;
  `POST /order` carries none until Django deduplicates order creation
  (#83). A redirect is never followed: it is `ErrUpstreamDown`, not
  retried, as in `internal/ucp/fetch.go`. Errors unwrap to sentinels
  (`ErrNotFound`, `ErrConflict`, `ErrThrottled`, `ErrValidation`,
  `ErrUpstreamDown`) — branch with `errors.Is`, never on status literals.
  An error answer is logged at debug with the route's template, the
  status and Django's `code`, never its detail.
  A failure while reading a body (a document, an error body) is
  `ErrUpstreamDown` like one before the headers, or `context.Canceled`
  for a caller that hung up; a redirect whose body is cut stays the
  redirect. A complete body that is not the JSON expected is a decode
  error. A context deadline is one of the gateway's own budgets, so a
  call it cuts is `ErrUpstreamDown`, logged. A caller that hung up (a
  cancelled context, shutdown included) gets an error wrapping
  `context.Canceled` only, not logged. A transport error names the
  route's template, like the metric label. Each attempt is counted once
  when it is over, by its outcome: `canceled` for a hang-up and `error`
  for an outage, before or after the headers, else the status; its
  duration covers the headers and the body read. Django never refuses a cart an
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
  had begun (the status writer keeps the first one). An MCP tool
  handler is the exception: the SDK runs it on a goroutine of its own,
  out of Recover's reach, so `mcptool.Add` recovers it. The panic is
  logged with its stack, the call's line has outcome `panic`, and the
  caller gets the tool error `unavailable`. A panic in the
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
  `FEED_FRESH_TTL` and `FEED_STALE_TTL`. Each document is written an
  item at a time into its gzip stream (`docStream`), which hashes and
  counts it for the ETag and `Meta.Size`: no feed is ever whole and
  uncompressed in memory, so a sweep costs the fetch window plus the
  gzip output, about a tenth of the documents (`memory_measure_test.go`,
  build tag `feedmem`, measures it). A failed generation is logged
  once with the request that started it, counted
  (`gateway_feed_generation_failures_total`) and marked
  (`ag:{schema}:feed:failed`, `failureBackoff`): until the mark expires
  or an invalidation drops it, a stale feed is served without a refresh
  and a missing one answers 503 without a sweep. A generation whose feed
  is gone again before it is read back is marked too; a read-back that
  failed is not (the feed was written): the request gets that error,
  its own context's when the crawler left, with no log line and no
  mark. A failure an invalidation overtook (its epoch moved) is not
  marked, and one that failed before reading the epoch always is. A revalidation reads the meta key
  and only TOUCHes the blob (Redis evicts keys one by one, and TOUCH,
  unlike EXISTS, keeps the blob's LRU clock with the meta key's): an
  entry without its blob is a miss and regenerates, and so is one whose
  blob is not whole: every body read checks the gzip header and the
  trailer's ISIZE against the stored size, in constant time, with no
  decoding. The gzip coding writes the blob as stored; only an identity
  GET decodes it, streamed under its declared length and never past it,
  with its last `holdBack` bytes written only once gzip has verified the
  CRC and the length. A decode that fails cuts the response short of
  `Content-Length` (net/http closes the connection), and the blob is
  logged and dropped (`dropBlob`, as the header check does), so the next
  request regenerates it. The drop is a compare-and-delete on the meta
  record read with the blob, so a blob a generation wrote since stays. One weak ETag covers the gzip and
  the identity body; a 200 (and a HEAD) declares `Content-Length` in
  either coding, a 304 carries only `ETag`, `Cache-Control` and `Vary`,
  and an error answer none of the three. The sweep is
  detached from the request (`context.WithoutCancel`) and names no
  caller to Django (`django.WithoutCaller`): it is the store's work, not
  the crawler's. Field limits are the strictest of Google, Meta and
  TikTok, named at the top of `rss.go`. A description drops what the
  storefront does not show: raw-text elements and any element carrying
  `hidden` (`plainText`). `internal/httphdr` parses
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
  `upstreamErr`, which renders an `mcptool.ToolError` as JSON text:
  Django's `code` and its `detail` (or its validation `fields`) for a
  refusal, `retryAfterSeconds` for a throttle, and a `hint`. A 403 is
  reported as `not_found`, and a 5xx or network failure only as
  `unavailable`: no upstream text beyond what Django wrote for a client.
  The other failures an agent acts on by their kind are `ToolError`s
  too: no linked account (`unauthorized`), a token without the tool's
  scope (`not_granted`, as on `/mcp/admin`), a refused coupon (its
  reason as the code) and the checkout store down
  (`errCheckoutUnavailable`, one value, `retryAfterSeconds` as an
  upstream outage). A cart tool's failure goes through `handlers.cartErr`,
  or `handlers.cartGoneErr` for a call on the cart itself, whose 404 is
  the cart gone (a line's or a product's 404 stays theirs): a cart the
  store no longer serves (`django.ErrCartUnreachable`, or that 404) is
  `not_found` on `/mcp`, whose agent owns its cart id and is told to
  create another with `create_cart`. In the chat it is the shopper's
  own browser cart, which a cart the model created would never replace
  and which the chat serves no `create_cart` for, so it is
  `cart_unreachable`: nothing changed, tell the shopper to reload the
  page and try again (`shopperCartUnreachable`, the same answer as a
  refused cart token). A checkout write that failed after
  another landed says so on top of its cause's hint (`withHint`). A list
  with nothing in it is `[]`, never `null`
  (`TestToolOutputsListNothingAsEmptyArrays`).
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
    the session lock in use, a session another request stored while
    this one ran (`checkout.ErrSessionChanged`), or an order being
    placed until `Flow.CompletionDeadline`. Past it the outcome was
    never recorded and stays so until the session expires: 502
    `order_outcome_unknown`, no `Retry-After`. A session in progress
    whose order is known (`Flow.PlacedOrder`) answers neither: 500
    `order_placed`, naming the order (`writePlacedUnsaved`).
  - A session that is not open refuses update and cancel
    (`refuseChange`): 405 and an `Allow` header once it is completed,
    canceled or awaiting its payment, the in-flight answer while its
    order is being placed. A canceled one refuses complete with 405. A
    complete of a session whose order is placed (completed, or awaiting
    payment) answers 200 with the session. The `continue_url` of one
    awaiting payment is the payment page the store opened for the
    order (`Session.PaymentURL`), as on UCP, and its message names the
    order; without a payment page it is the order's page.
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
    its key: a create that failed leaves no session and no record. A
    create that ran past its key's claim, with another create holding
    the key by then, discards its session and answers 409: one key
    answers one session. A create whose record could not be written
    (`storeIdem` returned an error that is not `errClaimLapsed`) does
    not know whether the key holds its response: it answers 503 and
    hands out no session, and does not discard it, because the record
    may name it. A retry of the key replays the session if it was
    recorded and creates anew if not. A 201 without its record would
    let that retry create a second session.
  - `completeError` answers every `checkout` and `django` sentinel
    deliberately (`TestCompleteErrorMapsEverySentinel` walks them): a
    refusal is a 4xx, and an order that exists or may is never
    answered as a failure.
- **A checkout no session is stored for is not "nothing happened".**
  The id does not say whether the checkout was never completed, placed
  an order and is no longer kept (expiry, eviction, a release that
  changes the session key), or
  never existed. Every "no session" answer is `mcpsrv.errCheckoutGone`
  (UCP) or `acp.msgSessionGone` (ACP's 404): an order may exist, check
  before starting another checkout, with the order id if the caller
  holds one and the store's order confirmation if not. Never answer a
  missing session with only "start a new one".
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
  session that is still open (`Session.Open`). After the flow they
  only settle their own key by the placement (ACP's body-hash record,
  UCP's completion claim), on a context the caller cannot cancel,
  bounded by the Django client's timeout, and map the error; neither
  saves the session again.
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
    its create stored and never handed out (the create's key turned
    out to be another request's), whose id no other request can hold.
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
    save can lose only to an order event, the one other writer of a
    session in progress: the completion then stores its outcome over
    the event's (`Flow.saveOverEvent`), keeping `completed` if the
    event said the order is paid. A last save that still cannot be
    stored, after its order was placed, is `ErrSessionNotSaved` with
    the key spent, and does not wrap `ErrSessionChanged`. A save Redis
    failed is `ErrUnavailable`, answered "the checkout records are
    unavailable" (`errCheckoutUnavailable`, ACP 503), never with
    Django's wording. The in-progress save and the save after the
    order call each get one more attempt when Redis fails them, with
    the same write: a first that was stored and lost only its reply is
    recognised as this call's.
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
    exists or may (`Session.keptLong`, `Session.ttl`): placed, a day
    after `PlacedAt`; its order call unanswered (`Session.Unconfirmed`,
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
    session when its order is placed (completed, or awaiting payment,
    on ACP too), in flight while it is being placed, canceled when it
    is. A session being placed is "in flight" only until
    `Flow.CompletionDeadline`; past it, both surfaces say the outcome
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
      refusal, a render, a replayed key. `finish` reads the stored
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
      `ag:{schema}:cs:v2:{id}:order`, the order's uuid) is for calls on
      the session, not events: Redis may evict it;
    - a payment-completed event also sets `completed`. No other event
      changes a status.

    It takes no session lock. Its save is the compare-and-set, and a
    lost one is a 503 that Django sends again. Django retries five
    times, each after a random delay between zero and 10, 20, 40, 80
    and 160 seconds (`push_order_event_to_gateway`, Celery's
    `retry_jitter`), so all five can fall inside the time a crashed
    pod's lock lasts, and a lock would then lose the event.
    `Flow.PlacedOrder` reads the same record for a session in progress,
    so a call on one whose completion placed the order and could not
    save it is answered with the order (`placedUnsavedErr`,
    `writePlacedUnsaved`).
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
  through `mcpsrv.Offered`. Which surface serves a tool is a trait on
  its definition too (`tool.on`): `chatOnly` (the coupon tools),
  `mcpOnly` (the six UCP tools, whose input requires `meta`, and
  `create_cart`) and `account`. With reviews off no rating or review count
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
  A checkout session is `ag:{schema}:cs:v2:{id}`, its lock that plus
  `:lock`, the order it placed that plus `:order`. The version is of how sessions are written, not only of their
  shape: bump it whenever an older pod writing the key its own way could
  undo what a newer one relies on (v1 pods wrote sessions unconditionally),
  so the two never share a session; a session under an older key is not
  found. `TestSessionKeyIsVersioned` pins it. A Redis write that depends
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

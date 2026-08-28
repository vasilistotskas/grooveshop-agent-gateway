# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with
code in this repository.

## Project Overview

GrooveShop **agent gateway** — a Go microservice that makes every tenant
storefront AI-shoppable. One binary serves five protocol surfaces on the
storefront's own domain (Traefik path-routes them here):

| Path | Surface |
|---|---|
| `POST /mcp` | MCP server (stateless streamable HTTP): the UCP checkout capability's five canonical tools + ergonomic commerce tools + account tools (`my_orders`, `my_loyalty_points`, `my_favourites`) |
| `GET /.well-known/ucp` | UCP business profile (spec 2026-08-25), incl. the `space.grooveshop.payments` handler |
| `/acp/*` | ACP agentic checkout REST (spec 2026-04-17) |
| `/feeds/*` | Product feeds: google.xml, meta.xml, tiktok.xml, acp.json |
| `POST /chat` | First-party shopping chatbot (SSE; OpenAI-compatible protocol via openai-go — Gemini free tier by default, any compatible provider via CHAT_BASE_URL) |
| `/internal/*` | Cluster-only: Django order-event push (shared-secret header) |

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
- `internal/config` — env config, fail-fast `Load()`. Per-tenant values come
  from Django `tenant/resolve` at request time, never env.
- `internal/identity` — optional-auth middleware on /mcp: a present
  `Authorization: Bearer` is verified against Django `/agent/me`
  (60s in-memory cache, SHA-256 keys); invalid tokens get 401 + the
  RFC 9728 `WWW-Authenticate` challenge; anonymous requests pass
  through. Account tools forward the bearer — Django enforces scopes.
- `internal/tenant` — resolver (process memory 5m → Redis `ag:tenant:{host}`
  → Django; singleflight; 60s negative cache; stale-if-error) + middleware
  (tenant from the **real Host header only** — inbound X-Forwarded-Host is
  deliberately ignored; spoofing it must never switch tenants).
- `internal/django` — typed API client. Every tenant-scoped call sends
  `X-Forwarded-Host: <tenant domain>` + `X-Forwarded-Proto: https` (+
  `X-Language`). GETs retry twice on 5xx/network; mutations never retry at
  transport level. Errors unwrap to sentinels (`ErrNotFound`, `ErrConflict`,
  `ErrThrottled`, `ErrValidation`, `ErrUpstreamDown`) — branch with
  `errors.Is`, never on status literals.
- `internal/httpmw` — recover, request-id, access log, RED metrics, per-IP
  rate limit. Metrics wraps the mux directly so `r.Pattern` is populated;
  tenant middleware runs *inside* routing (per route group), and enriches
  the access log via `httpmw.SetTenant` (the `Extras` holder).
- `internal/obs` — slog JSON to stdout, Prometheus registry, health.
  `/healthz` is process-only; `/readyz` gates on Redis ONLY — Django-down
  must never flip readiness (agents get actionable errors, not 502s).
- `internal/server` — mux assembly; new route groups register here.
- `testdata/fixtures/django/` — recorded upstream responses; DTO decode
  tests guard drift. Refresh when `grooveshop-django-api/schema.yml`
  changes a used endpoint.

## Conventions

- Hand-written minimal DTOs (camelCase tags); decode with `UseNumber()`,
  money as `json.Number` → integer cents internally, format at the edge.
- Business failures in MCP tools return `CallToolResult{IsError: true}`
  with actionable text — never a Go error (that becomes a protocol error).
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
  Unlike the surface gates it FAILS CLOSED — a payment behaviour must
  not switch on from a payload that never mentioned it. A gated
  `pay_way_id` is REFUSED, never ignored: ignoring would complete
  against a stale selection. These five tools are withheld from the
  chatbot (`internal/chat/bridge.go`) — their shapes target a platform
  generating calls, and chat hands over a checkout link instead.
- The service `schema` (OpenRPC) URL may appear ONLY while every method it
  defines for an ADVERTISED capability exists — the five checkout tools
  plus `get_order`. Declaring it asserts a machine-checkable contract.
  Its cart/catalog methods belong to capabilities the profile does not
  advertise, so a negotiating platform never calls them.
- `get_order` sources the required `checkout_id` from the gateway's own
  order index (`ag:{schema}:order:{uuid}`, retained a year — the link must
  outlive the session, which expires in 24h). An order placed on the web
  has no checkout to name and is refused with that explanation, never a
  fabricated id. `order.fulfillment` stays `{}`: an expectation needs a
  destination the order detail deliberately does not decode (PII), and an
  event needs a shipment timestamp upstream does not expose.
- Redis keys: `ag:{schema}:…`; tenant cache `ag:tenant:{host}`.
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

## Cross-repo contracts

- Django repo (`../grooveshop-django-api`): `tenant/resolve` endpoint shape,
  cart `X-Cart-Id` UUID header, guest checkout, `create_checkout_session`
  (Viva Smart Checkout URL). Payments: **Viva primary** (hosted
  authorization via UCP `requires_escalation`+`continue_url`), Stripe
  tokenized behind per-tenant flag.
- Nuxt repo: `/cart/claim?uuid=` claims a gateway-built cart into the
  browser session; `.well-known/mcp/server-card.json` points at `/mcp`;
  `.well-known/oauth-protected-resource[/mcp]` names the Django API as
  the OAuth authorization server (allauth.idp: auth-code + PKCE + DCR).
- Config gates: ACP and chat are per-tenant. The ACP platform bearer
  arrives as `acpBearerToken` and the chat model credential as
  `chatApiKey` on `tenant/resolve` — Django includes both only when the
  resolve call carries the gateway's `X-Internal-Token` (same shared
  secret as the order-event push). A tenant without a chat key gets a
  localized 404 from `/chat`; a tenant without an ACP token has ACP
  disabled — every bearer gets 401 from `/acp/*`.
- Infra repo: manifests under `manifests/app-constructs/grooveshop/base/`,
  path rules on the storefront ingress.

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with
code in this repository.

## Project Overview

GrooveShop **agent gateway** — a Go microservice that makes every tenant
storefront AI-shoppable. One binary serves five protocol surfaces on the
storefront's own domain (Traefik path-routes them here):

| Path | Surface |
|---|---|
| `POST /mcp` | MCP server (stateless streamable HTTP): commerce tools + UCP checkout tools + account tools (`my_orders`, `my_loyalty_points`) |
| `GET /.well-known/ucp` | UCP business profile (spec 2026-04-08) |
| `/acp/*` | ACP agentic checkout REST (spec 2026-04-17) |
| `/feeds/*` | Product feeds: google.xml, meta.xml, tiktok.xml, acp.json |
| `POST /chat` | First-party shopping chatbot (SSE, Claude via anthropic-sdk-go) |
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
- Redis keys: `ag:{schema}:…`; tenant cache `ag:tenant:{host}`.
- No tenant label on Prometheus metrics (unbounded cardinality) — tenant
  goes in logs.
- **No fallback/legacy/backward-compat code paths.** Environment differences
  are config values, not conditionals.
- Multi-tenant native: webside is tenant #1, never special-cased. Never
  hardcode production domains — they arrive via tenant config.
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
- Config gates: `ACP_BEARER_TOKEN` unset disables the ACP REST surface;
  `ANTHROPIC_API_KEY` unset disables /chat. Both log a startup warning
  instead of failing.
- Infra repo: manifests under `manifests/app-constructs/grooveshop/base/`,
  path rules on the storefront ingress.

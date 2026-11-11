[![CI](https://github.com/vasilistotskas/grooveshop-agent-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/vasilistotskas/grooveshop-agent-gateway/actions/workflows/ci.yml)

# grooveshop-agent-gateway

Go service that makes every GrooveShop store shoppable by AI agents: MCP commerce tools, UCP and
ACP agentic checkout, product feeds and the storefront's shopping chatbot.

GrooveShop is a multi-tenant e-commerce platform: a Django API, a Nuxt storefront, a media stream,
this agent gateway, Kubernetes infrastructure and a documentation site. The gateway is a protocol
adapter. Traefik path-routes its surfaces on each store's own domain, so an agent talks to
`https://<store-domain>/mcp`; the gateway resolves the store from the `Host` header and serves it
from the Django REST API. All commerce state lives in Django, sessions and caches live in Redis,
and the gateway holds no database and no payment-provider secrets. See the
[platform architecture](https://docs.grooveshop.space/dev/platform/architecture) (developer docs,
behind basic auth).

## Features

| Path | What it serves |
|---|---|
| `POST /mcp` | MCP server (stateless streamable HTTP): catalogue, cart, shipping, pickup-point, store-policy, order-tracking and restock/price-drop alert tools, the UCP checkout capability's canonical tools, and account tools for shoppers who linked their account over OAuth |
| `GET /mcp/server-card`, `GET /.well-known/ard.json`, `GET /.well-known/ai-catalog.json` | Agent discovery: the MCP Server Card, the ARD manifest and the AI Catalog, built from the tools the store actually serves |
| `GET /.well-known/ucp` | UCP business profile, with the platform's own payment handler (`space.grooveshop.payments`) |
| `/acp/*` | ACP agentic checkout REST, enabled per store |
| `/feeds/*` | Product feeds: `google.xml`, `meta.xml`, `tiktok.xml`, `acp.json` |
| `POST /chat` | Backend of the storefront's shopping chatbot (server-sent events): an OpenAI-compatible model looping over the same commerce tools |
| `/internal/*` | Cluster-only, shared secret: Django's order-status push (forwarded to agents as signed UCP order webhooks) and feed invalidation |
| `GET /healthz`, `GET /readyz`, `GET /metrics` | Liveness, readiness (gated on Redis only) and Prometheus metrics |

- **Tenant resolution.** The store comes from the real `Host` header only (an inbound
  `X-Forwarded-Host` is ignored), resolved through Django's `GET /api/v1/tenant/resolve` and cached
  in process memory and Redis. Every Django call then carries the store's domain.
- **Store features apply everywhere.** A feature the store has switched off (catalogue, cart,
  reviews, alerts, loyalty, favourites, promotions) is neither listed nor callable over MCP, not
  advertised in discovery, and refused by checkout, feeds and chat.
- **Payments.** Agent checkouts complete through Viva Wallet's hosted authorization (the agent
  hands the buyer a `continue_url`) or a collect-later method such as cash on delivery. Delegated
  card payments are refused for now.
- **Identity linking.** Django is the OAuth 2.1 authorization server; a bearer token on `/mcp` is
  verified against Django and forwarded only to the account tools.
- **Chat.** The model credential is per store (from `tenant/resolve`); the provider endpoint and
  default model are configuration (the Gemini API's OpenAI-compatible endpoint by default). Spend is
  bounded by per-IP rate limits and per-store and per-visitor turn quotas in Redis.

## Tech stack

- Go 1.27 (the exact toolchain is in `go.mod`), standard library `net/http` routing
- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), [openai-go](https://github.com/openai/openai-go)
- Redis
- golangci-lint (v2 config), testcontainers for the integration tests
- Distroless container image

## Getting started

Prerequisites: Go per `go.mod`, a running Django API and Redis (the GrooveShop dev stack), and
Docker for the integration tests.

```bash
git clone https://github.com/vasilistotskas/grooveshop-agent-gateway.git
cd grooveshop-agent-gateway
cp .env.example .env       # point DJANGO_BASE_URL and REDIS_URL at the dev stack
set -a; . ./.env; set +a   # the gateway reads the environment, not .env
go run ./cmd/gateway       # listens on :8080
```

`.env.example` lists every setting: the required ones first (startup fails without them), then the
optional ones with their defaults. Per-store values (ACP token, chat key, feature flags) are never
environment variables; they arrive from Django's `tenant/resolve`.

## Common commands

```bash
go build ./...                                        # Compile
go test ./internal/...                                # Unit tests
go test -tags integration ./internal/integration/...  # Integration tests (Docker)
golangci-lint run                                     # Lint
docker build -t agent-gateway .                       # Production image
```

CI also runs the tests with `-race`, which needs cgo; run it locally only if you have a C toolchain.

## Project layout

```
cmd/gateway/        # Wiring: config, clients, mux, graceful shutdown
internal/
├── server/         # Route registration and the internal endpoints
├── tenant/         # Host-based tenant resolver and middleware
├── django/         # Typed Django API client
├── mcpsrv/         # MCP server, tools, Server Card and discovery documents
├── ucp/            # UCP profile, negotiation, payment handler, order webhooks
├── acp/            # ACP checkout REST
├── checkout/       # Checkout sessions shared by UCP and ACP
├── feeds/          # Product feeds
├── chat/           # Chatbot handler, model loop, quotas
├── identity/       # Bearer verification for /mcp
├── storefront/     # Every link into the Nuxt storefront
└── ...             # config, httphdr, httpmw, httpsig, media, money, obs, text
contract/           # The gateway's Django consumer contract (OpenAPI)
testdata/           # Recorded Django responses for the decode tests
```

[`CLAUDE.md`](CLAUDE.md) holds the architecture, the cross-repository contracts and the
conventions; `.claude/rules/` holds the UCP, chat SSE and Django-contract rules.

## Deployment and release

A release is a pushed tag, not a merge. Merging to `main` runs CI (lint, tests, build) and
publishes nothing. Pushing a `vX.Y.Z` tag runs `release.yml`: the full test suite, then the image
build (version stamped into the binary) published to Docker Hub
(`gro0ve/grooveshop-agent-gateway`) and the GitHub Container Registry, then the GitHub Release.

```bash
git tag vX.Y.Z origin/main
git push origin vX.Y.Z
```

The cluster runs the version pinned in the GitOps repository `grooveshop-infrastructure`, whose
deploy gate checks `contract/django-consumer.openapi.json` against the pinned Django API; Argo CD
deploys it.

## Documentation

- [Agent gateway developer docs](https://docs.grooveshop.space/dev/gateway/) and the
  [agentic commerce flow](https://docs.grooveshop.space/dev/flows/agentic-commerce) (behind basic
  auth).
- [docs.grooveshop.space](https://docs.grooveshop.space): the platform documentation site.

## Related repositories

- [grooveshop-django-api](https://github.com/vasilistotskas/grooveshop-django-api): Django REST API, admin, and background jobs; the platform's source of truth
- [grooveshop-storefront-ui-node-nuxt](https://github.com/vasilistotskas/grooveshop-storefront-ui-node-nuxt): Nuxt server-rendered storefront for every store
- [grooveshop-media-stream](https://github.com/vasilistotskas/grooveshop-media-stream): NestJS image processing and caching service
- grooveshop-infrastructure (private): Kubernetes manifests deployed by Argo CD
- [grooveshop-docs](https://github.com/vasilistotskas/grooveshop-docs): the documentation site at docs.grooveshop.space

## Licence

Source-available under the [PolyForm Strict License 1.0.0](LICENSE.md): you may use it for
noncommercial purposes; you may not distribute it or make changes or new works based on it.
Commercial use needs a separate licence from the author.

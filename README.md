# grooveshop-agent-gateway

Agentic commerce gateway for the GrooveShop platform: one Go service that
makes every tenant storefront shoppable by AI agents.

- **MCP server** (stateless streamable HTTP) with commerce tools — search,
  products, categories, shipping options, ACS/BoxNow pickup points, carts,
  checkout handoff, order tracking, price/restock alerts.
- **UCP** business profile + checkout capability (Viva Wallet hosted
  authorization via `continue_url`; Stripe tokenized where enabled).
- **ACP** agentic checkout REST surface + product feed.
- **Product feeds** per tenant: Google Merchant, Meta, TikTok, ACP.
- **Shopping chatbot** backend (SSE) powering the storefront widget — an
  OpenAI-compatible model (via `openai-go`; the Gemini free tier by
  default, any provider via `CHAT_BASE_URL`) looping over the same
  commerce tools.

See `CLAUDE.md` for architecture and conventions. Configuration is
env-based — see `.env.example`.

## Quick start

```bash
cp .env.example .env   # point DJANGO_BASE_URL/REDIS_URL at the dev stack
go run ./cmd/gateway
```

Tests: `go test ./internal/...` (unit) and
`go test -tags integration ./internal/integration/...` (needs Docker).

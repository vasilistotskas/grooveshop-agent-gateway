---
paths:
  - "cmd/gateway/**"
  - "internal/httpmw/shutdown.go"
  - "internal/mcptool/**"
  - "internal/checkout/flow.go"
  - "internal/config/redis.go"
---

# Serving and shutdown (`cmd/gateway`)

Loads when working on the wiring, the shutdown cut, or what must fit in its final window. The code wins over this page.

- `cmd/gateway` — wiring: config → clients → mux, then `serve`
  (`serve.go`), the one piece of logic, tested. At SIGTERM (after the
  Deployment's 10s preStop sleep) in-flight requests drain for 10s with
  live contexts; then every request context is cancelled with cause
  `httpmw.ErrShuttingDown`. So is every MCP tool's, on both MCP
  servers and any protocol: the SDK ends a tool's context with its
  request only from protocol 2026-07-28, so `serve` also carries the cut
  as a value (`httpmw.WithShutdown`) and `mcptool.Add` ends each
  handler's context on it (`httpmw.UntilShutdown`). Then the final
  window begins: a stream writes its last event (the chat sends its
  `turn_failed` `error`), and an order completion that detached before
  the cut finishes (`completionTail`: `checkout.CompletionTail`, which
  is `CompletionBudget` and the settle steps after it, each on a
  `Store.Settling` context of one `UPSTREAM_TIMEOUT`: settle its key,
  release the session lock; then `answerSlack` to write the answer).
  Every Redis command ends at its context's deadline: the client is
  built with `ContextTimeoutEnabled` (`config.RedisOptions`), so
  go-redis sets each socket deadline to the earlier of the context's
  and its own read or write timeout, and retries nothing past it.
  `TestACompletionsStepsAfterItsOrderFitTheShutdownWindow` counts the
  steps a completion takes after its order and their bounds. Both
  surfaces hold it in their request: ACP's handler calls the flow, and
  the MCP SDK's stateless handler returns only once its tool has, on
  any protocol (`TestServeWaitsForAToolThatOutlivesItsRequest`). The window is the
  longer of the stream's 5s and that tail, 51s at
  `UPSTREAM_TIMEOUT=10s`. What is still open is closed, logged at
  warn, and the process exits 0: 61s after SIGTERM, so the
  infrastructure's `terminationGracePeriodSeconds` covers the 10s
  sleep, the 61s and a 5s margin (76s); each second of
  `UPSTREAM_TIMEOUT` adds 5s, and the infrastructure's pre-commit
  check refuses a grace period that does not follow. `ReadTimeout` (30s, half of
  Traefik's entrypoint `readTimeout`) bounds reading a request; the
  server clears it once the body is read, so streams outlive it.
  `IdleTimeout` (120s) sits above Traefik's 90s backend idle timeout.
  No `WriteTimeout`: it would cut every stream.

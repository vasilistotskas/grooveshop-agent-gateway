---
paths:
  - "internal/httpmw/**"
  - "internal/obs/**"
  - "internal/server/**"
---

# Middleware and observability (`internal/httpmw`, `internal/obs`)

Loads when working on the global middleware chain, logging, metrics or health. The code wins over this page.

- `internal/httpmw` — request-id, access log, rate limit, RED metrics,
  recover, outermost first (`server.withGlobalChain`), inside the
  server's `surfaceErrors` ("ACP errors are the spec's",
  `.claude/rules/acp.md`). Recover wraps
  the mux (through `server.unrouted`), so a handler panic is logged and still gets its access
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
  `r.Pattern` the mux sets. Its `method` label is one of net/http's
  method constants, any other method `other` (`methodLabel`). An
  inbound `X-Request-Id` keeps its visible ASCII only, trimmed and cut
  to 64 bytes (the 64 characters Django keeps of `X-Correlation-ID`),
  else one is minted.
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
  - **Every line is JSON, through one handler.** `cmd/gateway` builds
    the logger (`obs.NewLogger`) before the configuration loads, at
    info until `LOG_LEVEL` sets its `slog.LevelVar`; `env` is added
    once the configuration names it. So a boot failure is one JSON
    `fatal` line at error, and the process exits 1. go-redis's own
    lines (one logger per process, `redis.SetLogger`, set by
    `newRedis` with the client) go through it too
    (`obs.RedisLogger`): `redis client` at warn, its text in
    `detail`. One go-redis logs with a request's context names the
    request; most (a dial, the pool's upkeep) run on its own.
  - **A shared flight logs under its leader; each waiter's outcome
    under its own request.** Work that singleflight shares (the tenant
    resolve, the bearer probe, the feed sweep, the UCP profile fetch)
    runs on the context of the request that started it, detached
    (`context.WithoutCancel`): its own lines, the Django client's and
    Redis's among them, carry that request's id and store, once. What
    each request then answers is its own, and so is any line it logs
    about it (`store resolution failed`, `token verification failed`,
    its access line): logged with its own context, never inside the
    flight. `TestASharedResolveLogsEachOutcomeUnderItsOwnRequest` and
    `TestASharedProbeLogsEachOutcomeUnderItsOwnRequest` hold it.
  `/healthz` is process-only; `/readyz` gates on Redis and on the
  startup checks (`started`: the signing keys, `ucp.Keys.Checked`) —
  Django-down must never flip readiness (agents get actionable errors,
  not 502s). The server answers from boot while the check waits for
  Redis, so a pod booting during a Redis restart is unready, not
  crash-looping; order-webhook delivery starts once the check passed.

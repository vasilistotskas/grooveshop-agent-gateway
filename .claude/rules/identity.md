---
paths:
  - "internal/identity/**"
  - "internal/mcpsrv/account*.go"
  - "internal/mcpsrv/post.go"
  - "internal/adminmcp/identity.go"
---

# Bearer verification (`internal/identity`)

Loads when working on how /mcp and /mcp/admin verify a bearer, or /mcp's account challenge. The code wins over this page.

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
    - Django's 404: a 404. The store switched something off and this
      pod's copy has not caught up. For a surface (agent commerce,
      staff access) it is the answer the gateway's own gate gives once
      it has. For customer accounts it is not: Django names no gate in
      its 404, so until this pod's copy says accounts are off (about
      two `TENANT_CACHE_TTL`s at most) a request that still carries a
      bearer is a 404 on /mcp, and a guest's 200 after;
    - Django's 429: a 429 with its `Retry-After`;
    - a caller that hung up: a 503 with no error line;
    - anything else: a 503, logged with `server` (mcp or mcp_admin),
      under the refused request's own id, though it waited on a probe
      another request started (whose lines carry that one's,
      `.claude/rules/httpmw-obs.md`).
  - On /mcp a present `Authorization: Bearer` is verified against Django
    `/agent/me` (`NewShopperVerifier`; a 403 there is a valid token
    without the `profile` scope). On a store with `customerAccounts` off
    it is ignored, as Django ignores it: the call is a guest's, with no
    probe and no challenge. That store serves no account tool, so an
    anonymous call to one is the SDK's unknown tool, unchallenged, and
    an order that cannot be opened is not answered "sign in"
    (`orderNotOpened`).
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

---
paths:
  - "internal/tenant/**"
---

# Tenant resolution (`internal/tenant`)

Loads when working on the resolver, its caches or the tenant middleware. The code wins over this page.

- `internal/tenant` — resolver (process memory 5m → Redis `ag:tenant:<version>:{host}`
  → Django; singleflight; 60s negative cache; stale-if-error) + middleware
  (tenant from the **real Host header only** — inbound X-Forwarded-Host is
  deliberately ignored; spoofing it must never switch tenants). A store
  it cannot resolve is a 503 with an error line, unless the caller hung
  up (`context.Canceled`), which logs nothing. That line is each
  request's own, under its own id; the shared resolve's lines carry the
  id of the request that started it (`.claude/rules/httpmw-obs.md`).
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

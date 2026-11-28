---
paths:
  - "internal/config/**"
  - ".env.example"
---

# Configuration (`internal/config`)

Loads when working on the environment configuration. The code wins over this page.

- `internal/config` — env config, fail-fast `Load()`: a value that would
  boot and then misbehave refuses to boot. Unknown `ENV` or `LOG_LEVEL`
  (parsed once, into `Config.LogLevel`); a
  `DJANGO_BASE_URL` that is not an absolute http(s)
  URL with a host and no user info, query or fragment; an image URL
  template without `{assets_host}` and `{path}` or with another
  placeholder than those and `{schema}`; an `INTERNAL_EVENTS_SECRET`
  that is not a valid header value (control characters, leading or
  trailing blanks) or, in production and staging, shorter than
  `minInternalSecretLen`; a `SIGNING_KEY_ENCRYPTION_KEY` (required in
  every ENV, never logged) that is not standard base64 of exactly 32
  bytes; or, checked once Redis answers (`ucp.Keys.AwaitKEK`), one
  that opens none of the sealed signing keys Redis holds when it holds
  any, which ends the process; `FEED_STALE_TTL` shorter than
  `FEED_FRESH_TTL`; `ORDER_CREATE_ATTEMPTS` (3) above 5 (Django's
  budget of 10 order creations a minute per cart, of which a completion
  leaves the next complete half); `ORDER_CREATE_ATTEMPTS` and
  `ORDER_CREATE_RETRY_DELAY` (2s) whose waits between the attempts take
  `UPSTREAM_TIMEOUT` or more, since every attempt of a completion's
  order shares that one timeout (`checkout.OrderRetry`); any numeric
  setting at zero or below (among them
  `UCP_SIGNATURE_MAX_AGE`, 5m, and `UCP_SIGNATURE_CLOCK_SKEW`, 1m: the
  window a platform's request signature's `created` and `expires` are
  held to, when it carries them). `ENV`
  (production | staging | development | test) is folded into
  `AllowLocalWebhooks`, `PaymentHandlerEnv` and the secret's minimum
  length there so no handler compares environment strings.
  A refusal names the variable and never repeats a value that can
  carry a credential: the base URLs, the hosts and the image URL
  templates (user info), the internal secret, the key-encryption key,
  and a `REDIS_URL` that does not parse (`config.RedisOptions` drops
  go-redis's and net/url's reason too, which repeat the URL)
  (`TestLoadRefusalsNeverRepeatACredential`). The refusal is the boot
  failure's log line.
  The infra staging overlay sets `ENV=staging`: production network
  rules, sandbox handler label. `STOREFRONT_DEFAULT_LOCALE` (required, a
  bare language code) is the storefront build's `DEFAULT_LOCALE`
  (`i18n/locales.ts`): the language its unprefixed pages render on
  every tenant, whatever the tenant's own default.
  Per-tenant values come from Django `tenant/resolve` at request time,
  never env.

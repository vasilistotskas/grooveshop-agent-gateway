---
paths:
  - "internal/django/**"
  - "contract/**"
  - "testdata/fixtures/django/**"
---

# Django client and consumer contract

Loads when working on the Django client, its fixtures or the
contract files. The code wins over this page.

## The client (`internal/django`)

- `internal/django` — typed API client. `ResolveTenant` also reports
  whether Django recognised the internal token: Django adds `aiProfile`
  (possibly null) and `acpBearerToken` (possibly empty) only then, so
  their key presence is the answer, decoded beside `TenantConfig`
  (`tenantResolve`), never in it; one key without the other is a decode
  error. Every tenant-scoped
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
  (`core/middleware/idempotency.py`): today the chat's `add_to_cart`.
  `POST /order` carries none: Django keeps one order per agent
  checkout, by the checkout's id in the body (`agentCheckout`, believed
  beside the internal secret only), and answers a repeat with that
  order (201). Whether to send it again is the checkout flow's
  (`checkout.Flow`, `ORDER_CREATE_ATTEMPTS`), never the transport's. A redirect is never followed: it is `ErrUpstreamDown`, not
  retried, as in `internal/ucp/fetch.go`. Errors unwrap to the
  sentinels of `internal/django/errors.go` (`ErrNotFound`,
  `ErrConflict`, `ErrValidation`, `ErrThrottled`, `ErrUpstreamDown`,
  `ErrUnauthorized`, `ErrForbidden`, `ErrCartUnreachable`,
  `ErrCartTokenRefused`, `ErrCartHasUnpaidOrder`, `ErrTotalChanged`,
  `ErrCartChanged`) — branch with
  `errors.Is`, never on status literals.
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
  another, except `RemoveCartItem`: its 204 carries no cart to check
  (`TestCartCallsWithoutATokenNameTheCart`). A signed-in shopper's cart is reached only through the cart
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

## The consumer contract


`encoding/json` zero-fills a field Django stops sending, so a removed or
renamed field would serve wrong data (0 prices in feeds) with no error.
`contract/django-consumer.openapi.json` is the OpenAPI 3.0.3 document of
what the gateway needs from Django: every endpoint it calls, the query
parameters and bodies it sends, and only the response properties its DTOs
decode. It is the gateway's side of the infrastructure deploy gate
(`api-contract.sh`): `oasdiff breaking <contract> <Django schema.yml>`
for each gateway/Django pairing, as for the storefront, refusing a Django
release that drops, loosens or retypes one of those properties.

- **Generated, never edited.** `internal/django/contract_test.go` holds the
  call registry (client method, method, path, success status, query
  parameters, body/response DTO types, Django component names) and builds
  the document by reflecting over the DTOs. After changing a DTO or a call:
  `go test ./internal/django -run TestDjangoConsumerContract -update`, and
  commit the file; without `-update` a stale file fails the test.
- **Every call is registered.** `TestDjangoContractCoversEveryCall` parses
  the `Client` methods in the package's non-test `*.go` sources and fails when a `c.get`/`c.send` call is missing
  from the registry or differs from it (path, method, query keys and
  whether each is always sent, body and response types). Paths must be
  templated string literals (`/product/{id}`, concrete one in `rawPath`).
- **Required/nullable** come from the Go type: a value field is required
  and non-null; a pointer is optional, and nullable unless `omitempty`;
  `omitempty` is optional. Where Django's schema is looser than the Go type,
  the field says so with a tag the readers already honour (they treat the
  zero value as "not set"): `contract:"optional"`, `contract:"nullable"`,
  `contract:"untyped"` (Django declares a `oneOf` union),
  `contract:"undocumented"` (read but absent from Django's schema — e.g.
  the internal-only `aiProfile`). A sent value carries Django's declared
  format with `contract:"format=email|uri|double"`, and its Go type must
  marshal to Django's JSON type (`targetPrice` is a `json.Number`, sent as
  a number). Never tag a field to silence a real break: fix the DTO or the
  call.
- **How it is read.** oasdiff needs `--flatten-allof` (Django nests objects
  as `allOf`) and `--severity-levels contract/oasdiff-levels.txt`: removing
  an *optional* read property is an error too, and so is a request body
  property changing type or format. Informational only: enum values and
  unions (Django's to grow), request lengths/ranges/enums (Go types cannot
  state them), and query/path parameter types — parameters are declared as
  strings on purpose, since that is what the URL carries and Django's
  integer parameters accept it. Check a pairing locally with:

  ```bash
  docker run --rm -v "$PWD/contract:/c" tufin/oasdiff:<pinned> breaking \
    --flatten-allof --severity-levels /c/oasdiff-levels.txt \
    --allow-external-refs=false --fail-on ERR -- \
    /c/django-consumer.openapi.json \
    https://raw.githubusercontent.com/vasilistotskas/grooveshop-django-api/<tag>/schema.yml
  ```

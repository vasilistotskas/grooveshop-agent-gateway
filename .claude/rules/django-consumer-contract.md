---
paths:
  - "internal/django/**"
  - "contract/**"
---

# Django consumer contract

Moved from `CLAUDE.md`; loads when working on the Django client or the
contract files. The code wins over this page.

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
  the `client*.go` sources and fails when a `c.get`/`c.send` call is missing
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
  the internal-only `chatApiKey`). A sent value carries Django's declared
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

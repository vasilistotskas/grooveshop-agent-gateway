# Vendored UCP schemas

`2026-08-25/` is a verbatim copy of `source/schemas` from the
[UCP specification](https://github.com/Universal-Commerce-Protocol/ucp)
release tag **`v2026-08-25`** (Apache-2.0), the current stable release.

The directory name is the UCP protocol version and MUST match
`ucp.Version` in `internal/ucp/keys.go` — the contract tests resolve
`https://ucp.dev/schemas/*` refs against this directory, so a mismatch
would silently validate payloads against a spec version we do not
advertise.

To re-vendor a new release:

```bash
tag=v2026-08-25   # the release to vendor
curl -sSL "https://codeload.github.com/Universal-Commerce-Protocol/ucp/tar.gz/refs/tags/$tag" \
  | tar -xz --strip-components=1 -C /tmp/ucp ucp-${tag#v}/source/schemas
```

Then update `ucp.Version`, this directory's name, and the per-capability
`spec` page paths in `internal/ucp/profile.go` (those move between
releases — `/specification/checkout/` became
`/specification/shopping/checkout/` in 2026-08-25).

Validate the snapshot with the official CLI (`cargo install ucp-schema`):

```bash
ucp-schema lint testdata/schemas/ucp/2026-08-25
```

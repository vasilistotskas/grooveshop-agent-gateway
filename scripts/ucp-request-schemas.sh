#!/usr/bin/env bash
# Regenerates the canonical UCP tools' request schemas from the vendored
# spec (internal/ucp/spec/README.md). Run it after every re-vendor, and after
# changing the extensions composed in compose.json; the ucp package's tests
# refuse schemas generated from another spec tree.
#
# Needs the spec's own tool, ucp-schema (cargo install ucp-schema). Set
# UCP_SCHEMA to its path when it is not on PATH.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
ucp_schema="${UCP_SCHEMA:-ucp-schema}"
version="2026-08-25"
spec="$root/internal/ucp/spec/$version"
out="$root/internal/ucp/spec/requests"
remote="https://ucp.dev/$version/schemas"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# compose.json declares the capabilities the business advertises that add
# request members: checkout and the discount extension.
"$ucp_schema" compose "$out/compose.json" --schema-local-base "$spec" \
  --schema-remote-base "$remote" --output "$tmp/composed.json"
for op in create update complete; do
  "$ucp_schema" resolve "$tmp/composed.json" --request --op "$op" --bundle \
    --pretty --schema-local-base "$spec" --schema-remote-base "$remote" \
    --output "$out/checkout_$op.json"
done

# The digest names the spec tree and the composition the files were made
# from (ucp.TestRequestSchemasAreCurrent recomputes it): one line per file,
# "<sha256> <path>", sorted by path, then the sha256 of those lines.
digest() {
  local dir="$1"
  (cd "$dir" && find . -type f -printf '%P\n' | LC_ALL=C sort |
    while read -r path; do
      printf '%s %s\n' "$(sha256sum <"$path" | cut -d' ' -f1)" "$path"
    done)
}
{
  digest "$spec" | sed "s|^\([0-9a-f]*\) |\1 $version/|"
  printf '%s requests/compose.json\n' \
    "$(sha256sum <"$out/compose.json" | cut -d' ' -f1)"
} | sha256sum | cut -d' ' -f1 >"$out/digest"
"$ucp_schema" --version >"$out/generator"

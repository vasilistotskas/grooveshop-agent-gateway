#!/usr/bin/env bash
# Agent protocols smoke check. Production-safe: GET discovery and feeds, plus
# MCP initialize and tools/list. No carts, orders, forms or tool calls.
# Usage: bash agent-smoke.sh production|staging
set -u
# Relative, because Git Bash and Node disagree on what /tmp means on Windows.
T=$(mktemp -d ./agent-smoke.XXXXXX)
trap 'rm -rf "$T"' EXIT
case "${1:-production}" in
  production)
    SHOPS='webside.gr demo.grooveshop.space'
    AS_OF='api.webside.gr api-demo.grooveshop.space'
    OFF='fyteia.grooveshop.space'
    CONNECT='connect.grooveshop.space'
    ;;
  staging)
    SHOPS='demo-staging.grooveshop.space staging.webside.gr'
    AS_OF='api-demo-staging.grooveshop.space'
    OFF='fyteia-staging.grooveshop.space'
    CONNECT='connect-staging.grooveshop.space'
    ;;
  *) echo "usage: $0 production|staging"; exit 2 ;;
esac
fails=0
ok()  { printf '  ok    %s\n' "$1"; }
bad() { printf '  FAIL  %s\n' "$1"; fails=$((fails + 1)); }
code() { curl -s -m 25 -o /dev/null -w '%{http_code}' "$@"; }
want() { # want LABEL EXPECTED ACTUAL
  if [ "$2" = "$3" ]; then ok "$1 -> $3"; else bad "$1: expected $2, got $3"; fi
}
mcp() { # mcp HOST METHOD PARAMS
  printf '{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}' "$2" "$3" \
    | curl -s -m 25 -X POST "https://$1/mcp" \
      -H 'content-type: application/json' \
      -H 'accept: application/json, text/event-stream' \
      -H 'mcp-protocol-version: 2025-11-25' --data-binary @- \
    | sed -n 's/^data: //p;/^{/p'
}
INIT='{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"agent-smoke","version":"1"}}'
CORE='search_products get_product create_cart add_to_cart get_cart get_shipping_options get_payment_methods get_checkout_link track_order get_store_policies create_checkout get_checkout update_checkout complete_checkout cancel_checkout get_order my_orders'

for h in $SHOPS; do
  echo "== $h"
  for p in /.well-known/ucp /.well-known/ard.json /.well-known/ai-catalog.json /mcp/server-card /feeds/google.xml /feeds/meta.xml /feeds/tiktok.xml /feeds/acp.json; do
    want "GET $p" 200 "$(code "https://$h$p")"
  done
  want 'ACP without a bearer' 401 "$(code -X POST "https://$h/acp/checkout_sessions" -d '{}')"

  name=$(mcp "$h" initialize "$INIT" | node -e 'let s="";process.stdin.on("data",c=>s+=c).on("end",()=>{try{console.log(JSON.parse(s).result.serverInfo.version)}catch{console.log("")}})')
  if [ -n "$name" ]; then ok "MCP initialize, gateway $name"; else bad 'MCP initialize'; fi

  mcp "$h" tools/list '{}' > "$T/tools.json"
  curl -s -m 25 "https://$h/.well-known/ard.json" > "$T/ard.json"
  curl -s -m 25 "https://$h/.well-known/ucp" > "$T/ucp.json"
  if ! node -e '
    const fs = require("fs")
    const read = f => JSON.parse(fs.readFileSync(f, "utf8"))
    const tools = read(process.argv[2] + "/tools.json").result.tools.map(t => t.name)
    const missing = process.argv[1].split(" ").filter(t => !tools.includes(t))
    const ard = read(process.argv[2] + "/ard.json")
    const entry = (ard.entries || ard.resources || [ard])[0]
    const advertised = entry.capabilities || []
    const drift = advertised.filter(t => !tools.includes(t)).concat(tools.filter(t => !advertised.includes(t)))
    const ucp = read(process.argv[2] + "/ucp.json")
    const caps = Object.keys(ucp.ucp.capabilities)
    const say = (good, text) => console.log((good ? "  ok    " : "  FAIL  ") + text)
    say(!missing.length, "tools/list: " + tools.length + " tools" + (missing.length ? ", missing " + missing : ""))
    say(!drift.length, "ARD capabilities match tools/list" + (drift.length ? ": differ on " + drift : ""))
    say(caps.includes("dev.ucp.shopping.checkout") && caps.includes("dev.ucp.shopping.order"), "UCP " + ucp.ucp.version + ": " + caps.length + " capabilities")
    say(Array.isArray(ucp.keys) && ucp.keys.length > 0, "UCP signing keys: " + (ucp.keys || []).length)
  ' "$CORE" "$T" > "$T/out.txt" 2>/dev/null; then
    bad 'tools/list, ARD or UCP answered something unreadable'
  fi
  cat "$T/out.txt"
  fails=$((fails + $(grep -c FAIL "$T/out.txt")))
done

# The OAuth documents a client needs to link an account. Staging gates all but
# the demo store, so only that one is checked there.
for a in $AS_OF; do
  echo "== $a"
  shop=${a#api.}; shop=${shop#api-}
  want 'protected-resource metadata' 200 "$(code "https://$shop/.well-known/oauth-protected-resource/mcp")"
  want 'authorization server metadata' 200 "$(code "https://$a/.well-known/oauth-authorization-server")"
  keys=$(curl -s -m 25 "https://$a/.well-known/jwks.json" | node -e 'let s="";process.stdin.on("data",c=>s+=c).on("end",()=>{try{const k=JSON.parse(s).keys;console.log(Array.isArray(k)?k.length:"unreadable")}catch{console.log("unreadable")}})')
  if [ "$keys" = 0 ] || [ "$keys" = unreadable ]; then bad "JWKS serves no signing key ($keys)"; else ok "JWKS keys: $keys"; fi
done

# The store-admin connector (/mcp/admin). A store switches it on for its
# team (off by default): off, the endpoint and its metadata both answer 404;
# on, an anonymous call and a bogus token are both challenged and the
# metadata names the staff issuer on the connect host. Nothing is signed in
# to and no token is minted, so it is safe on production.
for h in $SHOPS; do
  echo "== $h/mcp/admin"
  prm=$(code "https://$h/.well-known/oauth-protected-resource/mcp/admin")
  if [ "$prm" = 404 ]; then
    want 'staff AI access off: /mcp/admin' 404 "$(code -X POST "https://$h/mcp/admin" -H 'content-type: application/json' -d '{}')"
    continue
  fi
  want 'metadata' 200 "$prm"
  headers=$(curl -s -m 25 -o /dev/null -D - -X POST "https://$h/mcp/admin" \
    -H 'content-type: application/json' -d '{}')
  want 'anonymous call' 401 "$(code -X POST "https://$h/mcp/admin" -H 'content-type: application/json' -d '{}')"
  case "$headers" in
    *"resource_metadata=\"https://$h/.well-known/oauth-protected-resource/mcp/admin\""*) ok 'challenge names the metadata' ;;
    *) bad 'challenge does not name the /mcp/admin metadata' ;;
  esac
  want 'a bogus token' 401 "$(code -X POST "https://$h/mcp/admin" -H 'authorization: Bearer not-a-token' -H 'content-type: application/json' -d '{}')"
  as=$(curl -s -m 25 "https://$h/.well-known/oauth-protected-resource/mcp/admin" | node -e 'let s="";process.stdin.on("data",c=>s+=c).on("end",()=>{try{const d=JSON.parse(s);console.log(d.resource+" "+d.authorization_servers[0])}catch{console.log("unreadable")}})')
  want 'metadata names the staff issuer' "https://$h/mcp/admin https://$CONNECT" "$as"
done
echo "== $CONNECT"
issuer=$(curl -s -m 25 "https://$CONNECT/.well-known/oauth-authorization-server" | node -e 'let s="";process.stdin.on("data",c=>s+=c).on("end",()=>{try{const d=JSON.parse(s);console.log(d.issuer+" "+d.scopes_supported.includes("admin:orders:read")+" "+("device_authorization_endpoint" in d))}catch{console.log("unreadable")}})')
want 'staff issuer metadata' "https://$CONNECT true false" "$issuer"
if [ "${1:-production}" = production ]; then
  want 'no admin on the connect host' 404 "$(code "https://$CONNECT/admin/")"
fi

# A store with the catalogue and cart off serves only the order tools: names,
# not a count, so a catalogue tool in place of one of them still fails.
for h in $OFF; do
  echo "== $h (cart and catalogue off)"
  want 'UCP profile is gone' 404 "$(code "https://$h/.well-known/ucp")"
  want 'ACP is gone' 404 "$(code -X POST "https://$h/acp/checkout_sessions" -d '{}')"
  want 'feeds are gone' 404 "$(code "https://$h/feeds/google.xml")"
  n=$(mcp "$h" tools/list '{}' | node -e 'let s="";process.stdin.on("data",c=>s+=c).on("end",()=>{try{console.log(JSON.parse(s).result.tools.map(t=>t.name).sort().join(" "))}catch{console.log("unreadable")}})')
  want 'tools/list' 'get_order get_store_policies my_orders track_order' "$n"
done

echo
if [ "$fails" = 0 ]; then echo 'SMOKE PASSED'; else echo "SMOKE FAILED: $fails check(s)"; exit 1; fi

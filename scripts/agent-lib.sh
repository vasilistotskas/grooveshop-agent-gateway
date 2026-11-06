# Client helpers for the agent protocols. Bodies go through stdin, never an
# argument: Windows turns non-ASCII arguments into "?", so Greek text would be
# lost before it reached curl. Source this file, then set:
#   GW     base URL of the gateway, e.g. https://demo-staging.grooveshop.space
#   STORE  the tenant host, e.g. demo-staging.grooveshop.space
# Optional: BEARER (OAuth access token on /mcp), ACP_TOKEN (the tenant's ACP
# bearer), UCP_PROFILE (URL of the calling platform's UCP profile).

# rpc METHOD [PARAMS_JSON]: one JSON-RPC call on /mcp, prints the response.
rpc() {
  local auth=() params="${2:-}"
  [ -n "$params" ] || params='{}'
  [ -n "${BEARER:-}" ] && auth=(-H "authorization: Bearer $BEARER")
  printf '{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}' "$1" "$params" \
    | curl -sS -m 40 -X POST "$GW/mcp" -H "Host: $STORE" \
      -H 'content-type: application/json' \
      -H 'accept: application/json, text/event-stream' \
      -H 'mcp-protocol-version: 2025-11-25' "${auth[@]}" --data-binary @- \
    | sed -n 's/^data: //p;/^{/p'
}

# tool NAME [ARGS_JSON]: calls an MCP tool, prints its result.
tool() {
  local args="${2:-}"
  [ -n "$args" ] || args='{}'
  rpc tools/call "{\"name\":\"$1\",\"arguments\":$args}"
}

# j EXPR: reads JSON on stdin, prints the JS expression over it (`d`).
# `t` is the parsed text of an MCP tool result, when there is one.
j() {
  node -e '
    let s = ""
    process.stdin.on("data", c => s += c).on("end", () => {
      const d = JSON.parse(s)
      let t
      try { t = JSON.parse(d.result.content[0].text) }
      catch { t = d.result?.structuredContent }
      const v = eval(process.argv[1])
      console.log(typeof v === "string" ? v : JSON.stringify(v))
    })' "$1"
}

# ucp_meta [IDEMPOTENCY_KEY]: the `meta` member every canonical tool takes.
ucp_meta() {
  local key=""
  [ -n "${1:-}" ] && key=",\"idempotency-key\":\"$1\""
  printf '{"ucp-agent":{"profile":"%s"}%s}' "$UCP_PROFILE" "$key"
}

# acp METHOD PATH [BODY_JSON] [IDEMPOTENCY_KEY]: prints the body, then
# `HTTP <code>`. Every POST needs a key; PATH is relative to /acp.
acp() {
  local idem=() body=()
  [ -n "${4:-}" ] && idem=(-H "Idempotency-Key: $4")
  [ -n "${3:-}" ] && body=(--data-binary @-)
  printf '%s' "${3:-}" | curl -sS -m 40 -X "$1" "$GW/acp$2" -H "Host: $STORE" \
    -H "Authorization: Bearer ${ACP_TOKEN:-}" \
    -H 'content-type: application/json' -H 'API-Version: 2026-04-17' \
    "${idem[@]}" "${body[@]}" -w '\nHTTP %{http_code}\n'
}

uuid() { node -e 'console.log(crypto.randomUUID())'; }

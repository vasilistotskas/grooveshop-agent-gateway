# Agent protocols runbook

Real flows that prove MCP, UCP and ACP work end to end. Every result below was observed, not assumed. The real-stack test (`internal/realstack`) runs every block marked `bash runbook` as written, against a real Django, so those commands cannot drift from the code. It runs nightly, before every gateway release, and when this page changes. Run the [smoke check](#1-smoke-check) after any production change.

Last run: the smoke check on 2026-11-07, gateway v1.37.2, on production (Django v3.228.3) and staging (Django v3.229.0). The staging flows on 2026-11-06, gateway v1.37.2, Django v3.228.1. The local flows on 2026-11-05, gateway v1.37.1, Django v3.226.1 plus #490.

## Where each flow may run

| Label | Meaning |
|---|---|
| **Smoke** | Safe on production: GET discovery and feeds, plus MCP `initialize` and `tools/list`. No carts, orders or forms. |
| **Staging** | Staging or local only. Never production. |
| **Local** | Local only: the flow logs errors on purpose, which would block `promote.py production`. |

## Results at a glance

| Flow | Label | Local | Staging |
|---|---|---|---|
| Smoke check | Smoke | n/a | PASS, also on production |
| M1 Browse and fill a cart | Staging | PASS | PASS |
| M2 Link an account, `my_*` | Staging | PASS | **Not finished:** passes up to the sign-in, which needs a person |
| M3 Discovery documents | Smoke | n/a | PASS, also on production |
| M4 A real agent shops | Staging | n/a | PASS |
| U1 UCP cash on delivery | Staging | PASS | PASS |
| U2 Hosted payment handoff | Staging | n/a | PASS |
| U3 Signed webhook | Local | PASS | n/a |
| U4 UCP cancel | Staging | PASS | PASS |
| A1 ACP cash on delivery | Staging | PASS | **Not run:** needs the tenant's token |
| A2 ACP cancel | Staging | PASS | **Not run:** needs the tenant's token |
| Failure flows | Local | PASS | n/a |
| S1 Store-admin smoke | Smoke | n/a | **Not run yet:** ships with gateway `/mcp/admin` |
| S2 Staff connects an AI app | Staging | Real-stack test | **Not run yet:** ships with gateway `/mcp/admin` |

The staging run logged no ERROR or CRITICAL, checked in VictoriaLogs. The same filter finds 2 over the last 7 days, so the check does match real errors.

A real sign-in, consent and PKCE (M2 without the staging password), and the refusal of a token bound elsewhere, run in the real-stack test.

The first staging flows ran on 2026-11-06, after production took the same versions on 2026-11-05. They were meant to run first, but the promote went ahead without them. They passed, so nothing needs undoing.

## 1. Smoke check

**Label:** Smoke. **Takes:** about a minute. **Purpose:** after a production change, prove every agent surface still answers and still agrees with itself.

It checks discovery, feeds, the ACP auth refusal, MCP `initialize` and `tools/list`, that the ARD capabilities match `tools/list`, the OAuth documents, the signing key, and that a store with the catalogue off stays off.

From the repository root:

```bash
bash scripts/agent-smoke.sh production   # or staging
```

**Expected:** `SMOKE PASSED`, exit 0.

**Observed:** PASS on production and on staging, 2026-11-07, gateway v1.37.2. Every OAuth store serves one signing key.

The stores differ on purpose, so do not "fix" these: on production, webside serves 25 tools and demo 26, because demo has loyalty on. A store with its catalogue off serves 4 tools and answers 404 on UCP, ACP and feeds.

**Official tools, also safe on production:**

```bash
# MCP Inspector, the official client
npx -y @modelcontextprotocol/inspector --cli https://webside.gr/mcp --transport http --method tools/list

# UCP profile against the spec, with the official CLI (cargo install ucp-schema)
curl -s https://webside.gr/.well-known/ucp > profile.json
ucp-schema validate profile.json \
  --schema internal/ucp/spec/2026-08-25/profile.json \
  --def business_schema --response --op read --json
```

**Expected:** 25 tools from Inspector, and `{"valid":true}`. **Observed:** both pass on webside, demo and demo-staging. All nine `spec` and `schema` URLs in the profile answer 200.

`ucp-schema compose` takes one root capability at a time. On the whole profile it stops with `multiple root capabilities found`, which is the tool, not a defect. With `dev.ucp.shopping.checkout` or `dev.ucp.shopping.order` alone it exits 0.

## 2. Setup

### Client helpers

Every flow below uses `scripts/agent-lib.sh`. Bodies go through stdin, because Windows turns non-ASCII arguments into `?`: Greek text sent as a `curl -d` argument arrives broken.

### Staging

```bash
. scripts/agent-lib.sh
export GW=https://demo-staging.grooveshop.space STORE=demo-staging.grooveshop.space
export UCP_PROFILE=https://raw.githubusercontent.com/vasilistotskas/grooveshop-agent-gateway/main/testdata/fixtures/ucp/runbook_platform_profile.json
```

The gateway fetches a platform profile only over public https, so staging uses the one in the public repository. It names no webhook, so nothing is delivered from staging.

Use the demo store. Its accounts are the ones testers may use, and it is the only staging store whose OAuth paths skip basic auth. `demo-staging` holds cash on delivery, BOX NOW pay on the go and Viva.

### Local

`kubectl port-forward` and a plain `go run` both fail on `/mcp`, because the MCP SDK answers 403 to a tenant `Host` on a loopback connection. Run the gateway in Docker, so requests arrive on a non-loopback address.

1. Give Django the gateway's secret and URL, and an OIDC key if you want `openid`. Add these to its environment, then restart it: `AGENT_GATEWAY_INTERNAL_SECRET`, `AGENT_GATEWAY_INTERNAL_URL=http://agent-gateway-local:8080`, `IDP_OIDC_PRIVATE_KEY`.
2. Start a private Redis and the gateway on Django's network:

```bash
NET=grooveshop-django-api_grooveshop-backbone
SECRET="<the AGENT_GATEWAY_INTERNAL_SECRET from step 1>"
docker run -d --name agent-redis-local --network $NET redis:8-alpine
docker run -d --name agent-gateway-local --network $NET -p 127.0.0.1:18080:8080 \
  -e ENV=development -e DJANGO_BASE_URL=http://backend:8000/api/v1 -e DJANGO_PUBLIC_HOST=localhost \
  -e REDIS_URL=redis://agent-redis-local:6379/0 -e INTERNAL_EVENTS_SECRET="$SECRET" \
  -e ASSETS_HOST=assets.localhost -e STAFF_CONNECT_HOST=connect.localhost \
  -e STOREFRONT_DEFAULT_LOCALE=el \
  "gro0ve/grooveshop-agent-gateway:$(git describe --tags --abbrev=0)"
```

3. Point the helpers at it. `ENV=development` lets the gateway fetch a profile over plain http, so serve one from any container on the same network:

```bash
. scripts/agent-lib.sh
export GW=http://127.0.0.1:18080 STORE=demo.localhost
export UCP_PROFILE="http://<your-container>:8090/profile.json"
```

Local gotchas:

- Use `127.0.0.1`, not `localhost`. `*.localhost` resolves to `::1` in Node, and Docker publishes on `127.0.0.1`.
- `celery_worker` may not be running, so a flow needing a task runs it directly: `push_order_event_to_gateway.run(order_id)` in `manage.py shell`, inside `schema_context("demo")`.
- Local data differs: `demo.localhost` has only flat-rate delivery, no Viva, and `fyteia.localhost` has its features on.

## 3. MCP

### M1. Browse and fill a cart

**Label:** Staging. **Purpose:** an agent shops as a guest, up to the store's checkout page.

```bash runbook
tool search_products '{"query":"θήκη","limit":3}' | j 't.products.map(p=>[p.id,p.name,p.effectivePrice].join(" | ")).join("\n")'
PID=$(tool search_products '{"query":"θήκη","limit":1}' | j 't.products[0].id')
tool get_product "{\"productId\":$PID}" | j 'JSON.stringify({name:t.name,price:t.effectivePrice,stock:t.stock})'
CART=$(tool create_cart | j 't.cartId')
tool add_to_cart "{\"cartId\":\"$CART\",\"productId\":$PID,\"quantity\":2}" | j 'JSON.stringify({total:t.total,items:t.items.length})'
tool get_shipping_options "{\"countryCode\":\"GR\",\"cartId\":\"$CART\"}" | j 'JSON.stringify(t.options)'
tool get_payment_methods | j 'JSON.stringify(t.methods.map(m=>m.providerCode))'
tool get_checkout_link "{\"cartId\":\"$CART\"}" | j 't.url'
```

**Expected:** products with VAT-inclusive prices, a cart total, delivery options, and a `/cart/claim?uuid=` link.

**Observed:** PASS on local (demo) and on staging, with the same tools driven by Claude Code in M4. `get_store_policies`, `get_product_reviews`, `update_cart_item`, `remove_cart_item`, `find_pickup_points`, `subscribe_product_alert` and `list_categories` also answered correctly.

### M2. Link an account and read `my_*`

**Label:** Staging. **Purpose:** a real client that starts with no token links a shopper's account by OAuth, then reads their orders.

The chain: an anonymous `my_orders` gets 401 with a challenge, the client reads the store's metadata, registers itself (DCR), sends the shopper to sign in, redeems the code with PKCE, and retries.

**To repeat it in Claude Code:**

```bash
claude mcp add --transport http grooveshop-demo https://demo-staging.grooveshop.space/mcp
```

Open Claude Code, ask it to show your orders at the store, then use `/mcp` to authenticate when it reports the server needs it. Your browser opens: answer the staging basic-auth prompt, sign in with a demo account, and approve the consent screen. Ask for your orders again. Remove it afterwards with `claude mcp remove grooveshop-demo`.

With MCP Inspector, from a real terminal so it can open your browser:

```bash
npx -y @modelcontextprotocol/inspector --cli https://demo-staging.grooveshop.space/mcp --transport http --method tools/call --tool-name my_orders
```

**Expected:** after you sign in, `my_orders`, `my_loyalty_points` and `my_favourites` return that shopper's data.

**Observed on local:** PASS, with the official MCP TypeScript SDK, a headless browser for the sign-in, and the token bound to `https://<primary domain>/mcp` as on production:

```
connected anonymously, tools/list: 26 tools
asked for scope: favourites:read loyalty:read orders:read profile
challenged: client started OAuth by itself (UnauthorizedError)
token granted, scope: favourites:read loyalty:read orders:read profile
my_orders OK, my_loyalty_points OK, my_favourites OK
```

**Observed on staging:** passes up to the sign-in. That step, and every step after it, was not run on staging.

| Step | Result |
|---|---|
| Anonymous `my_orders` | 401, with `WWW-Authenticate: Bearer scope="favourites:read loyalty:read orders:read profile", resource_metadata=...` |
| Protected-resource and authorization server metadata | 200 |
| JWKS | One key |
| Client registration (DCR) | 201. This left one client, `runbook-staging-check`, on staging |
| Token endpoint with a bad client | `invalid_client`, so the path is reachable |
| `/identity/o/authorize` | 401: sign-in stays behind basic auth, as intended |

The staging sign-in itself needs the staging basic-auth password in a browser. An automated session may not copy that password into a file, so the steps above are for a person.

### M3. Discovery documents

**Label:** Smoke. Covered by section 1: `/mcp/server-card`, `/.well-known/ard.json` and `/.well-known/ai-catalog.json` answer 200, and the ARD capabilities match `tools/list` exactly.

### M4. A real agent shops by itself

**Label:** Staging. **Purpose:** prove a real agent can find its way through the tools with no script. It leaves one guest cart and places no order.

Save as `mcp.json`:

```json
{"mcpServers":{"grooveshop":{"type":"http","url":"https://demo-staging.grooveshop.space/mcp"}}}
```

```bash
T=mcp__grooveshop__search_products,mcp__grooveshop__get_product,mcp__grooveshop__create_cart,mcp__grooveshop__add_to_cart,mcp__grooveshop__get_cart,mcp__grooveshop__get_shipping_options,mcp__grooveshop__get_checkout_link
claude -p "Find a USB-C cable at this store, tell me its price with VAT, put one in a cart, tell me the delivery options to Greece, and give me the link to finish checkout. Do not place an order." \
  --mcp-config mcp.json --strict-mcp-config --allowedTools "$T" --model haiku --no-session-persistence
```

**Expected:** a product, its price, delivery options and a `/cart/claim` link.

**Observed:** PASS on staging, 8 turns, no tool errors. It searched in English, retried in Greek by itself, and returned product 1 at €7.32, standard delivery at €3.00, and a working link.

## 4. UCP

UCP runs over `/mcp`: the five canonical checkout tools plus `get_order`, each carrying `meta.ucp-agent.profile`.

### U1. Buy with cash on delivery

**Label:** Staging. **Purpose:** discovery to a placed order, read back.

```bash runbook
BUYER='{"first_name":"Runbook","last_name":"Buyer","email":"runbook+ucp@example.com","phone_number":"+306912345678"}'
FUL='{"kind":"home_delivery","providerCode":"flat_rate","countryCode":"GR","city":"Αθήνα","zipcode":"10431","street":"Πανεπιστημίου","streetNumber":"12","regionId":"GR-14"}'
COD='{"instruments":[{"handler_id":"grooveshop_payments","type":"cash_on_delivery","selected":true}]}'
PID=$(tool search_products '{"query":"θήκη","limit":1}' | j 't.products[0].id')

R=$(tool create_checkout "{\"meta\":$(ucp_meta),\"checkout\":{\"line_items\":[{\"item\":{\"id\":\"$PID\"},\"quantity\":1}]}}")
ID=$(j 't.id' <<<"$R"); j 't.status' <<<"$R"
tool update_checkout "{\"meta\":$(ucp_meta),\"id\":\"$ID\",\"checkout\":{\"buyer\":$BUYER,\"fulfillment\":$FUL,\"payment\":$COD}}" | j 't.status'
KEY=$(uuid)
R=$(tool complete_checkout "{\"meta\":$(ucp_meta "$KEY"),\"id\":\"$ID\",\"checkout\":{\"payment\":$COD}}")
ORDER=$(j 't.order.id' <<<"$R"); j 't.status+" "+t.order.label' <<<"$R"
tool get_order "{\"meta\":$(ucp_meta),\"id\":\"$ORDER\"}" | j 'JSON.stringify({id:t.id,checkout_id:t.checkout_id,totals:t.totals.map(x=>x.type+"="+x.amount)})'
tool track_order "{\"orderUuid\":\"$ORDER\"}" | j 't.status'
```

**Expected:** `incomplete`, `ready_for_complete`, `completed` with an order, then the same totals from `get_order`.

**Observed on local:** PASS. Totals `subtotal=3958 fulfillment=300 fee=200 total=4458`, equal to the order's own breakdown. No ERROR logged.

**Observed on staging:** PASS, 2026-11-06, product 15, order 1186. `incomplete`, `ready_for_complete`, `completed`. Totals `subtotal=1600 fulfillment=300 fee=200 total=2100`, the same from `get_order`. Replaying `complete_checkout` with the same key returned the same order. `track_order` answered `PENDING`.

Leave out `regionId` and the checkout stays `incomplete`, with a message listing the valid codes. **Observed:** PASS.

### U2. Hosted payment hands off to the buyer

**Label:** Staging. **Purpose:** an online method cannot be paid by an agent, so the checkout must hand off to the provider's page. Never open the link, so no payment is made.

```bash
VIVA="<the viva_wallet id from get_payment_methods>"
BUYER='{"first_name":"Runbook","last_name":"Buyer","email":"runbook+ucp@example.com","phone_number":"+306912345678"}'
FUL='{"kind":"home_delivery","providerCode":"flat_rate","countryCode":"GR","city":"Αθήνα","zipcode":"10431","street":"Πανεπιστημίου","streetNumber":"12","regionId":"GR-14"}'
PID=$(tool search_products '{"query":"θήκη","limit":1}' | j 't.products[0].id')
R=$(tool create_checkout "{\"meta\":$(ucp_meta),\"checkout\":{\"line_items\":[{\"item\":{\"id\":\"$PID\"},\"quantity\":1}],\"buyer\":$BUYER,\"fulfillment\":$FUL,\"pay_way_id\":$VIVA}}")
ID=$(j 't.id' <<<"$R")
tool complete_checkout "{\"meta\":$(ucp_meta "$(uuid)"),\"id\":\"$ID\",\"checkout\":{}}" | j 'JSON.stringify({status:t.status,continue_url:t.continue_url,messages:(t.messages||[]).map(m=>m.code)})'
```

**Expected:** `requires_escalation`, with a `continue_url` on Viva's **demo** host and the code `requires_buyer_review`.

**Observed on staging:** PASS, 2026-11-06. `requires_escalation`, `continue_url` on `demo.vivapayments.com`, code `requires_buyer_review`. The link was not opened.

### U3. Signed order webhook

**Label:** Local. **Purpose:** an order event reaches the platform, signed so it can prove the sender. Staging has no public endpoint to receive one.

Serve a profile whose `dev.ucp.shopping.order` capability has `config.webhook_url` pointing at a small HTTP sink on Django's network, place an order as in U1, then push the real task in `manage.py shell`:

```python
from django_tenants.utils import schema_context
from order.models import Order
from order.tasks import push_order_event_to_gateway

with schema_context("demo"):
    push_order_event_to_gateway.run(Order.objects.get(uuid="<order uuid>").id)
```

Verify at the sink: `Content-Digest` must equal the SHA-256 of the raw body, and the RFC 9421 signature, rebuilt from `Signature-Input`, must verify as ES256 against the JWK with the same `kid` in the store's `/.well-known/ucp`.

**Observed:** PASS. Digest matches, signature valid, key published in the profile, `UCP-Agent` and `Webhook-Id` present, body is the full order entity.

### U4. Cancel

**Label:** Staging.

```bash runbook
PID=$(tool search_products '{"query":"θήκη","limit":1}' | j 't.products[0].id')
R=$(tool create_checkout "{\"meta\":$(ucp_meta),\"checkout\":{\"line_items\":[{\"item\":{\"id\":\"$PID\"},\"quantity\":1}]}}")
tool cancel_checkout "{\"meta\":$(ucp_meta "$(uuid)"),\"id\":\"$(j 't.id' <<<"$R")\"}" | j 't.status'
```

**Expected:** `canceled`. **Observed:** PASS on local and on staging.

## 5. ACP

ACP needs the tenant's `acp_bearer_token`, on staging as on local, every call an `API-Version` (the `acp` helper sends it; without it the answer is 400 `missing_api_version`), and every POST an `Idempotency-Key`. Without the token, every call gets 401:

```bash
export ACP_TOKEN="<the demo tenant's acp_bearer_token>"
```

### A1. Buy with cash on delivery

**Label:** Staging, with the demo tenant's token. **Purpose:** a platform creates a session, fills it, completes it and reads it back.

```bash runbook
PID=$(tool search_products '{"query":"θήκη","limit":1}' | j 't.products[0].id')
R=$(acp POST /checkout_sessions "{\"line_items\":[{\"id\":\"$PID\",\"quantity\":1}],\"currency\":\"EUR\",\"capabilities\":{}}" "$(uuid)")
tail -1 <<<"$R"
ID=$(sed '$d' <<<"$R" | node -e 'let s="";process.stdin.on("data",c=>s+=c).on("end",()=>console.log(JSON.parse(s).id))')
LINE=$(sed '$d' <<<"$R" | node -e 'let s="";process.stdin.on("data",c=>s+=c).on("end",()=>console.log(JSON.parse(s).line_items[0].id))')

ADDR='{"name":"Runbook Buyer","line_one":"Πανεπιστημίου 12","city":"Αθήνα","state":"Αττική","country":"GR","postal_code":"10431"}'
acp POST "/checkout_sessions/$ID" "{\"buyer\":{\"first_name\":\"Runbook\",\"last_name\":\"Buyer\",\"email\":\"runbook+acp@example.com\",\"phone_number\":\"+306912345678\"},\"fulfillment_details\":{\"name\":\"Runbook Buyer\",\"address\":$ADDR}}" "$(uuid)" | tail -1

acp POST "/checkout_sessions/$ID" "{\"selected_fulfillment_options\":[{\"type\":\"shipping\",\"option_id\":\"flat_rate:home_delivery\",\"item_ids\":[\"$LINE\"]}]}" "$(uuid)" | tail -1
acp POST "/checkout_sessions/$ID/complete" '{"payment_data":{}}' "$(uuid)" | tail -1
acp GET "/checkout_sessions/$ID" | sed '$d' | node -e 'let s="";process.stdin.on("data",c=>s+=c).on("end",()=>{const d=JSON.parse(s);console.log(d.status,d.order?.id,d.continue_url)})'
```

**Expected:** 201, 200, 200, 200, then `completed`, an order, and a `continue_url` on `/checkout/success/`.

**Observed on local:** PASS. `state` matched in capitals and without the accent. Totals as in U1.

**Not run on staging:** an automated session may not pull the tenant's token out of the database. The owner can run it with the demo token.

### A2. Cancel

**Label:** Staging. `acp POST "/checkout_sessions/$ID/cancel" '' "$(uuid)"` on an open session.

**Expected:** 200 `canceled`, and 405 on a second cancel, as the spec says. **Observed:** PASS on local.

## 6. Failure flows

**Label:** Local. These log warnings and errors on purpose. All observed on 2026-11-05, gateway v1.37.1 plus Django #490, with nothing logged at ERROR.

### Wrong tenant

| Case | Expected and observed |
|---|---|
| Unknown `Host` on `/mcp`, `/.well-known/ucp`, `/acp`, `/feeds` | 404 on all four |
| `X-Forwarded-Host` spoofed | Ignored: the real `Host` decides |
| One store's ACP token on another store | 401 |
| One store's checkout id on another store | "that checkout no longer exists" |
| Cart and catalogue off (staging fyteia) | 4 tools, and 404 on UCP, ACP and feeds |

### Stock

| Case | Expected and observed |
|---|---|
| `add_to_cart`, UCP or ACP create on an out-of-stock product | Refused: `insufficient_stock`, in the store's words |
| Quantity above 999 | Refused: `quantity_too_large` |
| Stock gone between ready and complete, UCP | Refused, naming the product. The session stays `ready_for_complete`, and a retry succeeds once stock returns |
| Stock gone, ACP | 400 `out_of_stock`, naming the product |

### Idempotency

| Case | Expected and observed |
|---|---|
| UCP complete or cancel with no key | Refused: "meta.idempotency-key is required" |
| UCP complete replayed, same key | Same order, none added |
| UCP complete again with a new key, after completing | Same order, none added |
| ACP POST with no key | 400 `idempotency_key_required` |
| ACP create replayed, same key and body | 201 with `Idempotent-Replayed: true` |
| ACP same key, different body | 422 `idempotency_conflict`, except a complete of an already completed session, which answers 200 with the session whatever the body (#128) |
| ACP create replayed after its session expired, same key and body | 201 with the original body and `Idempotent-Replayed: true` |
| ACP call while another holds the session (lock busy) | 409 `idempotency_in_flight` with `Retry-After` |
| ACP or UCP call whose session another call stored while it ran (a slow store can hold a call past its lock) | Nothing is written and no order is sent. ACP: 409 `idempotency_in_flight` with `Retry-After: 2`. UCP: an error saying the checkout was changed by another request. Sent again, the call reads the session as it is. Not exercised here: the integration suite holds the store's answer to produce it (`completion_fence_test.go`) |
| Two completes of one ready session at once, with one key or two | One order. The other complete answers as the row above, then, sent again, the placed session |
| ACP create that ran past its key's claim while another create took the key | 409 `idempotency_in_flight`; its own session is discarded, and the retry answers the other create's response |

### Invalid input

| Case | Expected and observed |
|---|---|
| Quantity 0 or -3 on `add_to_cart`, UCP, ACP | Refused by Django. An omitted quantity is 1 on `add_to_cart` and ACP, and refused by the schema on UCP |
| Unknown product, tool missing an argument, unknown tool | Clear refusal each time |
| Malformed JSON | 400 |
| Unknown cart, or a cart id that is not a UUID | "the shopper's cart could not be reached" |
| ACP with no line items | 400 `missing` at `$.line_items` |
| ACP with `currency: USD` | 400 `invalid` at `$.currency`, "This store sells in EUR." |
| ACP complete on an unready session | 400 `missing` |
| ACP or UCP tokenized card | Refused; no card data is accepted |
| Region a country does not have | Stays incomplete, and lists the codes |

### UCP negotiation

| Case | Expected and observed |
|---|---|
| No `meta`, or an empty profile | Refused |
| Profile not a URL, or 404 | JSON-RPC -32001 "UCP discovery failed" |
| Profile on another version | -32001 "Protocol version not supported" |
| Profile sharing no capability | `capabilities_incompatible` in the result |
| `pay_way_id` without the extension negotiated | Refused, never ignored |

### After the end

| Case | Expected and observed |
|---|---|
| Update or cancel a completed checkout | Refused, naming the order |
| Update a cancelled checkout | Refused |
| Cancel an ACP session twice | 405 |
| Update a completed or cancelled ACP session | 405 `conflict`, `Allow: GET, HEAD` |

## 7. Store-admin connector (`/mcp/admin`)

A store's owner and staff connect their own AI app to `https://<store>/mcp/admin` and read the store by their role. Every tool only reads. **A store switches it on** (`STAFF_AI_ACCESS_ENABLED`, off by default, served as `features.staffAiAccess`). While it is off, `/mcp/admin` and its metadata answer 404. Turning it off cuts connected apps at once: Django answers 404, and a tool call answers `access_off` (the gateway asks Django again before calling it missing data). Turning it on reaches the gateway within its 5-minute tenant cache. Their tokens come from Django's staff issuer on the platform's connect host (`connect.grooveshop.space`, staging `connect-staging.grooveshop.space`), never from the store's own issuer, which signs in customers. Design and the proposed write tools: `grooveshop-docs/src/dev/flows/staff-mcp.md`.

### S1. Smoke

**Label:** Smoke. It is part of `scripts/agent-smoke.sh`. A store with the switch off must answer 404 on both `/mcp/admin` and its metadata. For each store that has it on:
- an anonymous call and a bogus token both get 401;
- the challenge names `/.well-known/oauth-protected-resource/mcp/admin`;
- that document names the store's `/mcp/admin` as the resource and the connect host as the authorization server;
- the connect host serves the staff issuer's metadata (the staff scopes, no device flow), and on production, nothing else (`/admin/` is 404).

No one signs in and no token is minted.

### S2. A staff member connects an AI app

**Label:** Staging. **Purpose:** a real client with no token connects as a store's staff member and reads the store.

The chain:
1. The anonymous call gets 401 with a challenge, and the client reads the store's `/mcp/admin` metadata.
2. It registers with the staff issuer (DCR) and sends the person to the connect host.
3. The person signs in with their store-admin account (a superuser also gives their authenticator code) and approves only what their role opens.
4. The client redeems the code with PKCE and calls the tools.

**To repeat it in Claude Code:**

```bash
claude mcp add --transport http grooveshop-demo-admin https://demo-staging.grooveshop.space/mcp/admin
```

The demo store must have **Store Settings → `STAFF_AI_ACCESS_ENABLED`** on. Ask for the store's latest orders, then use `/mcp` to authenticate. Your browser opens on `connect-staging.grooveshop.space`: answer the staging basic-auth prompt, sign in with a staff account of the demo store, and approve. Ask again. Remove it afterwards with `claude mcp remove grooveshop-demo-admin`.

**Expected:**
- `whoami` names the store and the role.
- A STAFF role is never offered sales or profit; an ADMIN is never offered profit.
- `list_orders`, `get_order`, `list_products`, `find_customers`, `list_reviews` and `list_messages` answer.
- Review comments and messages come back as `{"untrusted": true, …}`.

**Real-stack test** (`internal/realstack/staff_test.go`), against a real Django:
- **End to end:** discovery, DCR, staff sign-in, a consent cut to a STAFF role, PKCE, then reads. `sales_report` is `not_allowed` and an area not approved is `not_granted`.
- **Refused:** an anonymous call is challenged with every read scope. A customer's token from the store's own issuer gets `invalid_token`. Someone off the store's team gets 403 at consent.

**Refusals Django proves** (`tests_mt/test_staff_agent_schema_binding.py`, real schemas):
- a customer token bound to the staff resource;
- a token connected to another store;
- staff of another store;
- leaving the team, which ends access at the next call;
- a staff token on the shopper surface.

## 8. Behaviour to know

These were checked and left as they are:

- **A repeat UCP cancel answers OK, even with a new key.** The spec says a business SHOULD send an error, but the gateway keeps no cancel keys, so it cannot tell a retry from a repeat, and nothing is at risk.
- **An unknown `pay_way_id` is accepted at update and refused at complete.** It is safe, because no order is placed, but it is reported late.
- **An unknown JSON-RPC method answers plain 400.** That is the MCP SDK's own answer, not -32601.
- **A guest-order UUID stops working once the order has an owner, or `GUEST_ORDER_ACCESS_DAYS` after the order.** `get_order` and `track_order` then answer that no order can be opened with that id, with the same words as for an unknown id. The gateway's own reads use a private route, so order webhooks keep working.
- **A placed checkout's €0 shipping row stays**, to match what the buyer approved. `get_order` leaves it out.
- **ACP accepts `quantity`, which the spec's request item does not have.** Omitted, it means one.
- **The challenge asks for every account scope at once.** Tokens are opaque to the gateway, so it could not step a grant up later. The shopper can narrow it on the consent screen.
- **Line ids change when a checkout completes.** Before the order they are cart item ids, after it product ids, as `get_order` names them. The cart ids no longer exist by then.
- **An update can leave the cart half-changed.** `SyncCartLines` writes line by line, so if Django refuses a later line, earlier ones stay. The update answers an error and the session is not saved. Line lists are full-state, so the next update corrects it, and a render always shows the real cart.
- **A checkout create fills its cart before the region lookup.** If the lookup fails, the lines stay in the cart and no session is saved. An update looks the region up first, so a failure there changes nothing.
- **Tool schemas use `type: ["null","array"]` and similar.** Inspector `--strict` warns about them, 124 times across 21 tools on 2026-11-06. It is legal JSON Schema and no client has failed on it, so nothing changed. Look again if a client fails.
- **If an order's outcome is unknown, the checkout cannot render.** When Django's answer to `POST /order` is lost, the session stays `complete_in_progress` with no order id, so a read goes to a cart that may be gone and fails. It is safe: it cannot place a second order. A platform that hangs up during `complete` does not cause it: the gateway waits for Django's answer without the platform, saves the checkout as placed and links the order, so the platform's retry with the same key answers the order. Not exercised here.

## 9. Bugs these flows found

Each is fixed at its root in its own PR, and all are live on production unless the table says otherwise. Fake-Django tests could not have found them, so the e2e fake now deletes the cart with the order and requires a region, as Django does, and the real-stack test runs the flows against Django itself.

| Bug | Effect on production | Fix |
|---|---|---|
| The gateway collected no region, and Django requires one | **No agent order to a Greek address could complete**, on UCP or ACP, since late September | gateway #56 |
| A placed checkout rendered from its deleted cart | The order existed, and the agent was told it failed and to retry | gateway #60 |
| A client with no token got a tool error, never a 401 | No client could start linking an account | gateway #61 |
| allauth compared a token's resource with the API URL | Every real client's token got 403 on all `my_*` tools | Django #490 |
| No OIDC signing key in any environment | The token endpoint crashed for any client asking for `openid` | infra #248 |
| Quantity below 1 read as 1 | An agent reducing a cart added to it | gateway #57 |
| ACP `complete` on an unready session blamed the store | A platform would abandon a checkout it could finish | gateway #58 |
| ACP ignored `currency` | A USD request silently got EUR amounts | gateway #59 |
| Staging basic auth covered the OAuth paths | OAuth could not be tested with a real client | infra #247 |
| A gift card counted as a discount in order totals | None today: no gateway order can carry a gift card | gateway #63 |
| A token for another resource got 403 instead of 401 | The gateway read it as a missing scope, so the client was never sent to link again | Django #495 |
| An error reading a placed order hid the order | The agent was told to retry an order that existed | gateway #64 |
| A lower-case country code found no region | Buyers from clients that send `gr` could not check out | gateway #65 |
| Discovery named whichever host was asked | A store's secondary domain advertised itself instead of the primary | gateway #66 |
| A client registering with no `scope` got `openid` alone | It could not link an account: asking for the commerce scopes ended in `invalid_scope` | Django #500 |
| Flower logged a clean stop as an uncaught error | No shopper effect, but the error blocked `promote.py production` | Django #502 |
| The gateway read its own orders through the guest route | From 14 days after an agent order, the platform stopped getting order webhooks | Django #504, gateway #72 |
| `get_order` called an expired guest order unavailable | The agent was told to retry something that could never work | gateway #72 |
| Every save restarted a placed checkout's lifetime | Repeated calls could keep a session alive indefinitely | gateway #72 |
| The UCP profile named whichever host was asked | A client reading it on a secondary domain was sent to a server it could not link an account on | gateway #71 |

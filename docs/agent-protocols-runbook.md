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
| U3 Signed webhook | Local | PASS before Django's outbox; **not run since** | n/a |
| U4 UCP cancel | Staging | PASS | PASS |
| A1 ACP cash on delivery | Staging | PASS | **Not run:** needs the tenant's token |
| A2 ACP cancel | Staging | PASS | **Not run:** needs the tenant's token |
| Failure flows | Local | PASS | n/a |
| S1 Store-admin smoke | Smoke | n/a | **Not run yet:** ships with gateway `/mcp/admin` |
| S2 Staff connects an AI app | Staging | Real-stack test | **Not run yet:** ships with gateway `/mcp/admin` |

The staging run logged no ERROR or CRITICAL, checked in VictoriaLogs. The same filter finds 2 over the last 7 days, so the check does match real errors.

A real sign-in, consent and PKCE (M2 without the staging password), and the refusal of a token bound elsewhere, run in the real-stack test.

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

That profile publishes no signing key, so calls made with it are unsigned, and `get_order` refuses them (U1, U5). To sign as a platform, make a throwaway key and a profile that publishes it, and publish that profile at a public https URL, for example as a secret gist (an unlisted gist is still served publicly by its raw URL):

```bash
KEY="$(mktemp -d)/platform.key"
go run ./cmd/ucp-sign keygen -key "$KEY" < testdata/fixtures/ucp/runbook_platform_profile.json > signed-profile.json
gh gist create --filename profile.json signed-profile.json   # prints the gist's URL
export UCP_SIGNING_KEY="$KEY"
export UCP_PROFILE="<the gist's raw URL of profile.json, from its Raw button>"
```

With `UCP_SIGNING_KEY` set, `scripts/agent-lib.sh` signs every `/mcp` call (`go run ./cmd/ucp-sign post`), so source it from the repository's root. Delete the gist, `signed-profile.json` and the key when done.

Use the demo store. Its accounts are the ones testers may use, and it is the only staging store whose OAuth paths skip basic auth. `demo-staging` holds cash on delivery, BOX NOW pay on the go and Viva.

### Local

`kubectl port-forward` and a plain `go run` both fail on `/mcp`, because the MCP SDK answers 403 to a tenant `Host` on a loopback connection. Run the gateway in Docker, so requests arrive on a non-loopback address.

1. Give Django the gateway's secret and URL, and an OIDC key if you want `openid`. Add these to its environment, then restart it: `AGENT_GATEWAY_INTERNAL_SECRET`, `AGENT_GATEWAY_INTERNAL_URL=http://agent-gateway-local:8080`, `IDP_OIDC_PRIVATE_KEY`.
2. Start a private Redis and the gateway on Django's network:

```bash
NET=grooveshop-django-api_grooveshop-backbone
SECRET="<the AGENT_GATEWAY_INTERNAL_SECRET from step 1>"
KEK="$(openssl rand -base64 32)" # seals the stores' signing keys in this Redis
docker run -d --name agent-redis-local --network $NET redis:8-alpine
docker run -d --name agent-gateway-local --network $NET -p 127.0.0.1:18080:8080 \
  -e ENV=development -e DJANGO_BASE_URL=http://backend:8000/api/v1 -e DJANGO_PUBLIC_HOST=localhost \
  -e REDIS_URL=redis://agent-redis-local:6379/0 -e INTERNAL_EVENTS_SECRET="$SECRET" \
  -e SIGNING_KEY_ENCRYPTION_KEY="$KEK" \
  -e ASSETS_HOST=assets.localhost -e STAFF_CONNECT_HOST=connect.localhost \
  -e STOREFRONT_DEFAULT_LOCALE=el \
  "gro0ve/grooveshop-agent-gateway:$(git describe --tags --abbrev=0)"
```

3. Point the helpers at it. `ENV=development` lets the gateway fetch a profile over plain http, so serve one from any container on the same network. To sign, serve the profile `go run ./cmd/ucp-sign keygen` printed and set `UCP_SIGNING_KEY`, as for staging:

```bash
. scripts/agent-lib.sh
export GW=http://127.0.0.1:18080 STORE=demo.localhost
export UCP_PROFILE="http://<your-container>:8090/profile.json"
```

Local gotchas:

- Use `127.0.0.1`, not `localhost`. `*.localhost` resolves to `::1` in Node, and Docker publishes on `127.0.0.1`.
- `celery_worker` may not be running, so a flow needing a task runs it directly in `manage.py shell`, inside `schema_context("demo")`: the order events waiting in Django's outbox go out with `agent.outbox.deliver_due()`.
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

A store with customer accounts off (`features.customerAccounts` false) has no `my_*` tools: `tools/list` leaves them out, a call to one is the MCP SDK's `unknown tool` error (`-32602`) with no challenge, and a bearer the client still sends is ignored. For up to about two `TENANT_CACHE_TTL`s after the store switches accounts off, a pod whose copy still says on verifies that bearer, Django answers 404, and the request is a 404; requests without a bearer are not affected.

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

```text
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

`get_order` serves only a request signed per RFC 9421 by the platform whose signed `create_checkout` placed the order, and a checkout created signed answers only its signer. So U1 runs signed: set `UCP_SIGNING_KEY` and a `UCP_PROFILE` that publishes its key (section 2).

### U1. Buy with cash on delivery

**Label:** Staging. **Purpose:** discovery to a placed order, read back.

```bash runbook
BUYER='{"first_name":"Runbook","last_name":"Buyer","email":"runbook+ucp@example.com","phone_number":"+306912345678"}'
FUL='{"methods":[{"type":"shipping","line_item_ids":[],"destinations":[{"street_address":"Πανεπιστημίου 12","extended_address":"2ος όροφος","address_locality":"Αθήνα","postal_code":"10431","address_country":"GR","address_region":"GR-14"}],"groups":[{"id":"parcel","selected_option_id":"flat_rate"}]}]}'
COD='{"instruments":[{"handler_id":"grooveshop_payments","type":"cash_on_delivery","selected":true}]}'
PID=$(tool search_products '{"query":"θήκη","limit":1}' | j 't.products[0].id')

LINES="[{\"item\":{\"id\":\"$PID\"},\"quantity\":1}]"

R=$(tool create_checkout "{\"meta\":$(ucp_meta),\"checkout\":{\"line_items\":$LINES}}")
ID=$(j 't.id' <<<"$R"); j 't.status' <<<"$R"
tool update_checkout "{\"meta\":$(ucp_meta),\"id\":\"$ID\",\"checkout\":{\"line_items\":$LINES,\"buyer\":$BUYER,\"fulfillment\":$FUL,\"payment\":$COD}}" | j 't.status'
KEY=$(uuid)
R=$(tool complete_checkout "{\"meta\":$(ucp_meta "$KEY"),\"id\":\"$ID\",\"checkout\":{\"payment\":$COD}}")
ORDER=$(j 't.order.id' <<<"$R"); j 't.status+" "+t.order.label' <<<"$R"
tool get_order "{\"meta\":$(ucp_meta),\"id\":\"$ORDER\"}" | j 'JSON.stringify({id:t.id,checkout_id:t.checkout_id,totals:t.totals.map(x=>x.type+"="+x.amount)})'
tool track_order "{\"orderUuid\":\"$ORDER\"}" | j 't.status'
```

**Expected:** `incomplete`, `ready_for_complete`, `completed` with an order, then the same totals from `get_order`. Unsigned, the same block stops at `get_order` with `signature_missing`.

An update replaces the checkout, so it carries the lines, the buyer and the fulfillment every time (an omitted fulfillment is no delivery); the payment selection is kept when omitted. The delivery is the spec's fulfillment extension (`dev.ucp.shopping.fulfillment`, which the runbook profile declares): one `shipping` method, its destination the address with the house number in `street_address` (Django splits it) and a floor or flat in `extended_address`, and the carrier as its group's `selected_option_id`, one of the group's `options`. A checkout picked up at a point chooses the `pickup` method instead: its `line_item_ids` name the lines, and its `selected_destination_id` is one of the points it lists (the ACS points near `checkout.context.postal_code`) or a `destinationId` from `find_pickup_points` (`{"methods":[{"type":"pickup","line_item_ids":[...],"selected_destination_id":"boxnow:GR:25"}]}`); the order carries no address. A point the store's catalogue does not hold is refused at completion with `pickup_point_unknown` at `$.fulfillment.methods[1].selected_destination_id`. A street line Django cannot split is refused at completion: the answer is the checkout `incomplete`, with the error `street_number_missing` (or another of Django's codes for the line, such as `street_number_is_postcode`) at `$.fulfillment.methods[0].destinations[0].street_address`, and nothing is placed. A `complete_checkout` never places an order at a total the checkout did not show. One whose instrument selects another method than the checkout holds places nothing and answers `incomplete` with the error `payment_method_changed`. One that names the first method of a checkout that holds none places the order when the method adds no fee for that cart; when it adds one, it places nothing and answers `incomplete` with the error `payment_fee_added`, which says the fee and the new total. Either way the platform selects the method with `update_checkout`, which shows the total, and completes again; the same idempotency key can be used.

**Observed on local:** PASS. Totals `subtotal=3958 fulfillment=300 fee=200 total=4458`, equal to the order's own breakdown. No ERROR logged.

**Observed on staging:** PASS, 2026-11-06, product 15, order 1186. `incomplete`, `ready_for_complete`, `completed`. Totals `subtotal=1600 fulfillment=300 fee=200 total=2100`, the same from `get_order`. Replaying `complete_checkout` with the same key returned the same order. `track_order` answered `PENDING`.

Leave out `address_region` and the checkout stays `incomplete`, with a message listing the valid codes. **Observed:** PASS (on the earlier request shape, where the member was `regionId`).

**Real-stack test** (`internal/realstack/order_create_test.go`), against a real Django: a pickup order at an ACS Smartpoint (paid on delivery) and at a BOX NOW locker (paid online), each selected by its id, shown by its catalogue name and delivered to the point's catalogue address; a street line with no house number refused `street_number_missing` at the line until the address changes; and a cart written in Django after its checkout showed it refused with Django's own 409 `cart_changed`, then placed after a read. The stack gives its throwaway store placeholder carrier credentials for this, with every carrier URL leading nowhere (`internal/realstack/testdata/seed.py`).

### U2. Hosted payment hands off to the buyer

**Label:** Staging. **Purpose:** an online method cannot be paid by an agent, so the checkout must hand off to the provider's page. Never open the link, so no payment is made.

```bash
VIVA="<the viva_wallet id from get_payment_methods>"
BUYER='{"first_name":"Runbook","last_name":"Buyer","email":"runbook+ucp@example.com","phone_number":"+306912345678"}'
FUL='{"methods":[{"type":"shipping","line_item_ids":[],"destinations":[{"street_address":"Πανεπιστημίου 12","extended_address":"2ος όροφος","address_locality":"Αθήνα","postal_code":"10431","address_country":"GR","address_region":"GR-14"}],"groups":[{"id":"parcel","selected_option_id":"flat_rate"}]}]}'
PID=$(tool search_products '{"query":"θήκη","limit":1}' | j 't.products[0].id')
R=$(tool create_checkout "{\"meta\":$(ucp_meta),\"checkout\":{\"line_items\":[{\"item\":{\"id\":\"$PID\"},\"quantity\":1}],\"buyer\":$BUYER,\"fulfillment\":$FUL,\"pay_way_id\":$VIVA}}")
ID=$(j 't.id' <<<"$R")
tool complete_checkout "{\"meta\":$(ucp_meta "$(uuid)"),\"id\":\"$ID\",\"checkout\":{\"payment\":{}}}" | j 'JSON.stringify({status:t.status,continue_url:t.continue_url,messages:(t.messages||[]).map(m=>m.code)})'
```

**Expected:** `requires_escalation`, with a `continue_url` on Viva's **demo** host, the error `payment_required` with severity `requires_buyer_input` naming when the link stops being payable, and `expires_at` when the store will have decided the order (`paymentDecidedBy`). The spec requires `payment` on a complete; `{}` names no instrument, so the method the checkout holds is used.

**Observed on staging:** PASS, 2026-11-17 (v1.46.0). `requires_escalation`, `continue_url` on `demo.vivapayments.com`, `payment_required` / `requires_buyer_input`, the link payable until 14:31:26Z and `expires_at` 15:15:00Z, the same on a second read. The link was not opened.

### U3. Signed order webhook

**Label:** Local. **Purpose:** an order event reaches the platform, signed so it can prove the sender. Staging has no public endpoint to receive one.

Serve a profile whose `dev.ucp.shopping.order` capability has `config.webhook_url` pointing at a small HTTP sink on Django's network, and place an order as in U1. Its "created" event waits in Django's outbox (`AgentOrderEvent`); send what is due in `manage.py shell`:

```python
from django_tenants.utils import schema_context
from agent.outbox import deliver_due

with schema_context("demo"):
    print(deliver_due())  # {'delivered': 1}
```

Verify at the sink: `Content-Digest` must equal the SHA-256 of the raw body, and the RFC 9421 signature, rebuilt from `Signature-Input`, must verify as ES256 against the JWK with the same `kid` in the store's `/.well-known/ucp`. `Webhook-Id` is the outbox row's id and `Webhook-Timestamp` its `occurred_at`. Answer 503 from the sink once: the row stays `pending`, and its next attempt carries the same two headers.

**Observed:** PASS on the design before the outbox (Django's Celery push). Digest matches, signature valid, key published in the profile, `UCP-Agent` and `Webhook-Id` present, body is the full order entity. Not run since.

### U4. Cancel

**Label:** Staging.

```bash runbook
PID=$(tool search_products '{"query":"θήκη","limit":1}' | j 't.products[0].id')
R=$(tool create_checkout "{\"meta\":$(ucp_meta),\"checkout\":{\"line_items\":[{\"item\":{\"id\":\"$PID\"},\"quantity\":1}]}}")
tool cancel_checkout "{\"meta\":$(ucp_meta "$(uuid)"),\"id\":\"$(j 't.id' <<<"$R")\"}" | j 't.status'
```

**Expected:** `canceled`. **Observed:** PASS on local and on staging.

### U5. A signed request through the edge

**Label:** Staging. **Purpose:** a platform's signature covers the request's `Host`, path, `Content-Type` and body bytes, so Cloudflare and Traefik must pass them to the pod unchanged, or every signed call fails.

Set up a signing platform as in section 2, then:

```bash
. scripts/agent-lib.sh
export GW=https://demo-staging.grooveshop.space STORE=demo-staging.grooveshop.space
export UCP_SIGNING_KEY="<the key file keygen wrote>" UCP_PROFILE="<the raw URL of the published profile>"
UNKNOWN=00000000-0000-4000-8000-000000000000
tool get_order "{\"meta\":$(ucp_meta),\"id\":\"$UNKNOWN\"}" | j 't.messages[0].code'
UCP_SIGNING_KEY= tool get_order "{\"meta\":$(ucp_meta),\"id\":\"$UNKNOWN\"}"
```

**Expected:** `not_found` from the signed call: the signature verified, and the order is not this platform's. The unsigned one prints the JSON-RPC error `signature_missing`, which the gateway sends with HTTP 401. A signed call answered `signature_invalid` or `digest_mismatch` means something on the way changed what was signed; the gateway's log line does not say what, so compare the request as sent with what the pod received.

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
| Unknown `Host` on `/mcp`, `/.well-known/ucp`, `/acp`, `/feeds` | 404 on all four; on `/acp` the spec's `Error`, `not_found` |
| `X-Forwarded-Host` spoofed | Ignored: the real `Host` decides |
| One store's ACP token on another store | 401 |
| One store's checkout id on another store | "that checkout no longer exists here", as for any id no session is stored for: the answer says an order may exist and how to check, never only "start a new one" (ACP: 404 `not_found`, with the same meaning in its message) |
| A UCP checkout's id on an ACP route, or an ACP session's id on a UCP tool | The same missing-session answer: a session answers only on the surface that created it. It is not written or locked. `checkout_surface_test.go` in the integration suite |
| Cart and catalogue off (staging fyteia) | 4 tools, and 404 on UCP, ACP and feeds. ACP's is the spec's `Error`, `not_found`, the same answer as an unknown `/acp/` path |
| Any answer on `/acp/*` before the ACP handler (store unavailable 503, rate limit 429 with `Retry-After`, a panic's 500, the mux's 404 and 405 with `Allow`) | The spec's `Error` (`type`, `code`, `message`) as `application/json`, with the same status, `Retry-After` and `Allow`. `TestACPAnswersBeforeTheHandlerAreSpecErrors` in `internal/server` |

### Stock

| Case | Expected and observed |
|---|---|
| `add_to_cart`, UCP or ACP create on an out-of-stock product | Refused: `insufficient_stock`, in the store's words |
| Quantity above 999 | Refused: `quantity_too_large` |
| Stock gone between ready and complete, UCP | Refused, naming the product. The session stays `ready_for_complete`, and a retry succeeds once stock returns |
| Stock gone, ACP | 400 `out_of_stock`, naming the product |
| UCP or ACP create or update the store refuses at a lookup (region, payment, an item id that is not a number) | Refused before the cart is written (an id that is a number but names no product is refused by the store when the line is added, after the cart exists): a create leaves no guest cart in Django, an update leaves the cart and the stored session as they were. `TestRefusedPaymentLeavesTheCartAlone`, `TestRegionLookupFailureLeavesTheCartAlone` and `TestARefusedACPCallLeavesTheCartAlone` in the integration suite |

### Idempotency

| Case | Expected and observed |
|---|---|
| UCP complete or cancel with no key | Refused: "meta.idempotency-key is required" |
| UCP complete or cancel with a key that is not 1 to 255 visible ASCII characters (longer, a space, non-ASCII) | Refused: "meta.idempotency-key must be 1 to 255 visible ASCII characters (no spaces), such as a UUID". Nothing is read or written. `ucp_idempotency_key_test.go` in the integration suite |
| UCP complete replayed, same key | Same order, none added |
| UCP complete again with a new key, after completing | Same order, none added |
| ACP POST with no key | 400 `idempotency_key_required` |
| ACP POST with a key longer than 255 characters | 400 `invalid` at `Idempotency-Key`, "must be 1 to 255 characters, such as a UUID". No Redis key is built from it. ACP's spec makes the key an opaque string of at most 255 characters, so any key within that is accepted (UCP's key is bounded to visible ASCII, `checkout.ValidIdempotencyKey`) |
| ACP complete with no key, on a completed, canceled or in-progress session | 400 `idempotency_key_required`: the key is checked before the session |
| ACP create, update, complete or cancel replayed, same key and an equivalent body (members reordered, `1.0` for `1`) | The original status and body, byte for byte, with `Idempotent-Replayed: true`, whatever the session became since. Kept 24 hours unless the record is lost (a flush or a restore of the gateway's Redis); after that the key runs as a new request. A replay takes no session lock |
| ACP same key, different body (`null` where the first had no member counts as different) | 422 `idempotency_conflict`, on every operation |
| ACP create replayed after its session expired, same key and body | 201 with the original body and `Idempotent-Replayed: true` |
| ACP POST refused (400, 404, 405) | The message ends "This Idempotency-Key keeps this answer for 24 hours: send a new attempt with a new key." The key keeps the refusal and replays it, `Allow` included, even after what caused it is gone (`TestAnACPRefusalIsReplayedAfterItsCauseIsGone`). A new attempt needs a new key |
| ACP POST answered 409 `idempotency_in_flight`, 429, or a 5xx | The key is free: the same key runs again as a new request. A complete answered 502 `order_outcome_unknown` sent again with its key resumes the completion (section 10). Except a complete answered 500 `order_placed` for an order whose session was not saved: that key keeps the answer and replays it (departs from the spec's §6.5, see `.claude/rules/acp.md`) |
| ACP update or cancel that was saved and then answered a 5xx (its session could not be rendered), retried with the same key | 200 with the session, and the session is not saved again: the retry finds its own mark in it (`Session.AppliedBy`). A cancel is not answered 405 for its own cancel, also when its save was stored and Redis lost the reply: that request is answered 200 at once |
| ACP call while another holds the session (lock busy) | 409 `idempotency_in_flight` with `Retry-After` |
| ACP or UCP call whose session another call stored while it ran (a slow store can hold a call past its lock) | Nothing is written and no order is sent. ACP: 409 `idempotency_in_flight` with `Retry-After: 2`. UCP: an error saying the checkout was changed by another request. Sent again, the call reads the session as it is. Not exercised here: the integration suite holds the store's answer to produce it (`completion_fence_test.go`) |
| Two completes of one ready session at once, with one key or two | One order. The other complete answers as the row above, then, sent again, the placed session |
| ACP create that ran past its key's claim while another create took the key | 409 `idempotency_in_flight`; its own session is discarded, and the retry answers the other create's response |

### One checkout per cart

A cart has one live checkout: two on one cart could each place an order, because Django keeps the cart of an order that awaits its online payment. The session that holds a cart is named in `ag:{schema}:checkoutcart:{cartId}`, written with the session and kept as long as it. Only UCP `create_checkout` with `cart_id` can name a cart that already has a checkout. Not run by hand here. `checkout_cart_test.go` in the integration suite covers every row below against a fake Django, and the real-stack test (`internal/realstack/cart_checkout_test.go`) covers, against a real Django: one checkout per cart in any spelling of its id, a cart kept while its order awaits payment, a canceled unpaid order freeing the cart, and Django's own refusal of a second order.

| `create_checkout` with `cart_id`, by the checkout that holds the cart | Expected |
|---|---|
| None, or one that is completed or canceled | A new checkout, which holds the cart. Django usually removes a guest cart its order emptied, so after a completed checkout the cart is usually gone: see the row for a cart Django no longer serves |
| One this server cannot read: the cart's record names a session that is not stored under this release's key (another release's, while a key move rolls out, or one Redis lost) | Refused, `conflict`: "held by a checkout whose records this server cannot read, so an order may exist", do not start another checkout before checking, and where to look with the checkout id or order id the caller holds. The record holds the cart until it ends with the lifetime its checkout gave it: half an hour after an open one's last save, up to a day otherwise (section 9). A record that ended with its session between the gateway's two reads frees the cart, and one another create took meanwhile is decided again |
| Any, for a cart Django no longer serves (removed with its order or by a sign-in, or never made) | `not_found`: "that cart no longer exists: the shopper may have checked it out on the store's site, so an order may exist. Check with the shopper before building another cart with create_cart. No checkout was created". Nothing is left stored. A read of a checkout whose cart is gone says the same of the checkout, and to check before starting another checkout. A Django outage on that read is `unavailable`, never this answer |
| An open UCP checkout | It is canceled and the new checkout holds the cart: the newest create wins. The old id reads `canceled`, and a complete of it is refused. A create the store refuses (region, payment) cancels nothing. Its discount codes go to the cart only after the new checkout holds it. A create that fails after it took the cart (that write, the save after it, the render) discards the new checkout, and the old one stays canceled |
| An open UCP checkout another call is at work on (an update, a completion) | Not canceled under that call. The create looks again, three times at most, then answers `conflict`: "another request is at work on this cart's checkout", send it again shortly |
| An open ACP session | Refused, `conflict`: "started outside this server". The ACP session is not touched. (One whose unpaid order Django canceled is canceled like a UCP one: nothing is left of it to finish) |
| One whose order is being placed, or was sent and never confirmed | Refused, `conflict`: "an order exists or may", do not start another checkout, and where to look with the checkout id or order id the caller holds. Nothing is looked up or written |
| One whose order awaits its payment | The order is read from Django. Still unpaid and not canceled: refused, `conflict`: "placed an order, which awaits its payment", with the same guidance. Canceled (Django cancels an unpaid online order when its payment window ends): that checkout is canceled and the new one holds the cart. Not readable: `unavailable`, retry. No longer in Django: refused as unpaid |
| Several creates on one cart at once | One checkout on the cart is left that is not canceled. A create that finds the holder changed three times answers `conflict`, "send it again shortly" |
| `cart_id` that is not a UUID | Refused: "cart_id is not a cart id". Every spelling of a UUID names the same cart |

No answer gives the caller another checkout, or names one or its order.

| Complete of a checkout whose cart has an earlier order awaiting its payment (Django refuses it, `cart_has_unpaid_order`) | Expected |
|---|---|
| UCP `complete_checkout` | The tool error carries `"code":"cart_has_unpaid_order"`, Django's sentence as `detail`, and the hint "an order placed from this checkout's cart already awaits its payment, so this checkout placed no order: do not start another checkout for this cart …". Counted as `refused` |
| ACP complete | 400 `invalid_request`, code `cart_has_unpaid_order`: "An order placed from this session's cart already awaits its payment, so this session placed no order. Do not create another session for this cart …" |
| Both | No order. The checkout stays `ready_for_complete`. Once Django lets the order through, a complete places it: on UCP with the same key (its claim is free), on ACP with a new key (the key keeps its refusal) |

A cart Django just made (UCP `line_items`, an ACP create) is taken the same way. If it already has a record, no session is handed out: UCP `unavailable`, ACP 503, and an error line "a cart just made is already in a checkout".

Two limits. The record can be lost while its session lives (a flush or a restore of the gateway's Redis, which never evicts). Every read of a session touches the record, and the session's next save writes a lost one again; a session nobody reads or saves (one left awaiting its payment) can still lose it, and that cart can then get a second checkout. What refuses the second order then is Django: an agent's order from a cart whose earlier order still awaits its online payment is answered 400 with `code` `cart_has_unpaid_order` (django-api #693). The same refusal follows a shopper who opened a checkout's `continue_url`, placed a hosted-payment order on the storefront and left it unpaid. No order is placed and the checkout stays `ready_for_complete`; UCP's key is free, ACP's keeps the refusal. Over UCP the platform gets that code, Django's sentence, and a hint: this checkout placed no order, start no other checkout, and once the store cancels that order when its payment window ends, this checkout can be completed. Over ACP it gets 400 `invalid_request`, code `cart_has_unpaid_order`, with a message of the same meaning. Neither names the earlier order (Django names none). Once that order is paid, Django empties the cart and the next complete is refused `cart_empty`; once it is canceled unpaid, the next complete places this checkout's order. And the release that adds the record moves the session key (section 9), because an older pod saves a session without keeping its cart's record alive: while it rolls out, a checkout an older pod stored is not seen by a newer pod, and the rule holds among the checkouts the newer pods store.

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
| Update or cancel a checkout whose order awaits its payment (UCP `requires_escalation`) | Refused, naming the order: the buyer could still pay a checkout that read cancelled. No ACP session reaches it: ACP completes only with a collect-later pay way, checked again when the order is placed |
| Update, complete or cancel while the order is being placed | UCP: "a completion of this checkout is in progress or unconfirmed; check its status shortly". ACP: 409 `idempotency_in_flight` with `Retry-After` |
| Complete once that completion can no longer be running and the store never confirmed the order, or at once when the session is stored `unconfirmed` (the completion stored that as it ended) | The order is sent again with the checkout's id, and the store answers the order the checkout placed, or places it if the first call never reached it: the checkout ends completed (or, on UCP, awaiting its payment) with one order. A refusal leaves it ready again. A store that still does not answer gets the next row's answer. `TestAnUnconfirmedOrderIsAnsweredAsOne`, `TestResumeEndsAnUnconfirmedCheckoutWithItsOneOrder` |
| Update or cancel in that state, or a complete the store still does not answer | UCP: `order_outcome_unknown`, "the store has not confirmed whether this checkout's order was placed. Call complete_checkout on this checkout again shortly, with a new meta.idempotency-key", with `retry_after` and not to start another checkout. ACP: 502 `order_outcome_unknown` with `Retry-After`, "Complete this session again after Retry-After" |
| A read (`get_checkout`, ACP GET) of a session in progress or unconfirmed | It renders while the cart still reads: status `complete_in_progress`, no link back to the cart. On UCP the text beside the status is the same guidance the other tools give for that state. When the cart no longer reads (it may have gone with the order), the read gets the answer of the rows above |
| Any call on a session in progress whose order is known: the completion placed it and could not save the session | UCP: "the order was placed but checkout state could not be saved; track it with track_order using orderUuid …". ACP: 500 `order_placed`, naming the order. The order was recorded beside the session when Django returned it; the next order event records it on the session |
| Complete of a cancelled checkout | UCP: "is canceled and cannot be completed". ACP: 405 |
| Update a cancelled checkout | Refused |
| Cancel an ACP session twice | 405 |
| Update a completed or cancelled ACP session | 405 `conflict`, `Allow: GET, HEAD` |
| An update, complete or cancel that outlived its lock, when another request stored the session meanwhile (a complete that placed the order, a payment event) | Answered by what is stored now, never from what the call loaded and never with what Django said about the cart (it is empty, a line is gone, the stock is short: the other request's doing). A complete gets the placed session; an update or cancel the refusal that names the order; a session still open "changed by another request" (ACP 409, UCP `conflict`); one no longer stored the missing-session answer; one that cannot be read "unavailable". `checkout_snapshot_test.go`, `checkout_truth_test.go`, `checkout_after_order_test.go` |
| An update that outlived its lock before its first cart write, when the session was stored meanwhile | It writes nothing to the cart, and is answered as in the row above. `checkout_snapshot_test.go` |
| A complete whose cart changed since the checkout showed it (an update that outlived its lock wrote it under the completion, a line swapped at the same price) | Nothing is placed: Django refuses the order's `expectedCartDigest`. UCP: the tool error `cart_changed`; ACP: 400 `cart_changed`. The checkout stays ready, and a read shows the cart now; complete after it. `checkout_quote_test.go` |
| A complete whose session could not be marked in progress (Redis) | No order is sent. UCP: `unavailable`, "the store's checkout records are temporarily unavailable". ACP: 503 `unavailable`. The key is free for the same call |
| ACP create whose idempotency record could not be written | 503 `unavailable`, and the session is not handed out. The same key replays it if the record was written after all, and creates anew once the claim ends if not |

A session that may have an order is kept 24 hours: one that awaits its payment or is completed ends 24 hours after its order was placed; one whose order call went unanswered (`unconfirmed` in the stored session) has no placement time and ends 24 hours after that was stored. A session only marked in progress, like an open one, lives 30 minutes after its last save: that is every session whose completion is running, and one whose outcome Redis could not store afterwards (entry 11).

Every UCP and ACP checkout response carries `expires_at`, the session's `expiresAt`: until then the checkout can be read, and it never moves earlier than a value already sent. For an open or completed checkout it is when its session ends (above). For one awaiting its payment it is the order's `paymentDecidedBy`, when Django's unpaid-order sweep will have decided it (a platform polls until then and reads the decision at it), or, when nothing will decide it (a payment being processed, a charge that did not match, the sweep switched off), when the session ends. A session is kept until its `expiresAt` if that comes after its 24 hours. Each read of a checkout awaiting its payment reads its order: canceled, the checkout is `canceled` (the shopper owes nothing; a payment that landed after the cancel is refunded by the store); paid, it is `completed`. The order events do the same from the status and payment status they carry. A link whose time to pay ran out is not replaced: the checkout says so and points at the order, which Django cancels by `expires_at`. A checkout id no session is stored for is the spec's error response on UCP (`ucp.status: error`, `not_found`, `unrecoverable`), with the same text as before.

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
- **Tool schemas use `type: ["null","array"]` and similar.** Inspector `--strict` warns about them, 124 times across 21 tools on 2026-11-06. It is legal JSON Schema and no client has failed on it, so nothing changed. Look again if a client fails.
- **If an order's outcome is unknown, the checkout cannot render.** When Django's answer to `POST /order` is lost, the session stays `complete_in_progress` with no order id, so a read goes to a cart that may be gone and fails. It is safe: it cannot place a second order. A platform that hangs up during `complete` does not cause it: the gateway waits for Django's answer without the platform, saves the checkout as placed and links the order, so the platform's retry with the same key answers the order. Not exercised here.

## 9. Before deploying a release that moves the checkout session key

**Applies to:** every release that moves the checkout session key (`sessionKey` in `internal/checkout/store.go`). Seven have so far:

| Release | Key | Why an older pod must not share a session |
|---|---|---|
| The one that fixed gateway #84 | `ag:{schema}:cs:{id}` to `ag:{schema}:cs:v2:{id}` | It writes a session without checking what the key holds |
| The one with gateway #141's cart record | `cs:v2` to `cs:v3` | It saves a session without keeping its cart's record alive, so the record would expire under a session that still holds the cart |
| The one that binds a checkout to the platform that signed its creation | `cs:v3` to `cs:v4` | It knows nothing of that platform: it would answer any caller for the session, and its save would drop the platform |
| The one that publishes `expires_at` (gateway #108) | `cs:v4` to `cs:v5` | It knows nothing of the expiry a session published: its save would drop it, and a later answer could give an earlier one |
| The one that prices from Django's quote (gateway #107) | `cs:v5` to `cs:v6` | It knows nothing of the total a session showed: its save would drop it, and the session would then place no order until it is read again |
| The one that resumes an unanswered order (gateway #83) | `cs:v6` to `cs:v7` | It sends an order without its checkout's id, so Django cannot answer a repeat with it: a new pod resuming that session would place a second order |
| The one that sends the cart digest a checkout showed and takes UCP's fulfillment extension (gateway #142, #109) | `cs:v7` to `cs:v8` | It knows nothing of the cart digest a session showed, nor of a delivery's address line 2 or a pickup point without an address: its save would drop them (the session would then place no order until it is read again), and it reads a one-line street as incomplete |

**The pods it replaces must already answer a missing session truthfully.** Since the release with gateway #138, a checkout no session is stored for is answered "an order may exist, check before starting another", never only "start a new one". While a key-moving release rolls out, a pod of either kind then answers truthfully for a session it cannot see. Do not deploy one onto pods older than that.

The new pods do not read sessions the old pods stored, and the reverse, on purpose: an old pod writes a session its own way (the table above), so the two must never share one. What a platform sees, by the session's state:

| The session | During the rollout (pods of both releases) | After it |
|---|---|---|
| Open (`incomplete`, `ready_for_complete`), stored by either kind | Found on pods of its kind. On the other kind: the missing-session answer; a platform that follows it finds no order and starts again | An old one is gone for good, a new one is found everywhere |
| `complete_in_progress` or `requires_escalation`: an order exists, or may | Found on pods of its kind (do not start another checkout; the order; its payment page). On the other kind: the missing-session answer, which says an order may exist and how to check. A platform that starts another checkout anyway orders twice: for a hosted payment the first order's cart is still there | An old one: the missing-session answer until the platform stops asking. The order webhooks go through the order link, which does not move. From the `cs:v4` release on, `get_order` serves an order only to the platform that signed its checkout's creation, so an order placed before that release (its link names no platform) answers `not_found` on the new pods for good: the platform still must not start another checkout, and the order is read from the shopper's order confirmation, or with `track_order` by its id |
| `completed`, `canceled` | As above; only the session's own read is lost | As above |
| A payment event for a session a **new** pod stored, delivered to an **old** pod | The old pod finds the order link, looks under the old key, finds no session and answers 204, which is final for Django. The order webhook still goes out. That session stays `requires_escalation` until it ends, 24 hours after its order was placed: `get_checkout` keeps showing the payment page of a paid order, `get_order` shows the truth | Cannot happen |

**A cart held by the other release's checkout.** The record of the checkout that holds a cart (`ag:{schema}:checkoutcart:{cartId}`) is not versioned: it names its session by id. A pod that finds a record whose session it does not read under its own key holds the cart until the record ends (`CartHolder.Unread`, since the `cs:v7` release), with the lifetime its session gave it: half an hour after an open checkout's last save, up to a day for one that ended or whose order exists or may. A `create_checkout` naming such a cart is answered `conflict`: held by a checkout this server cannot read, an order may exist, check before starting another. That holds for the old release's checkouts during the rollout and after it, until their records end, and it is the same rule for a session Redis lost. Pods older than the `cs:v7` release read such a record as a free cart, so while one serves beside a new pod, a create on it can take a cart a new pod's checkout holds. The Deployment rolls its two replicas one at a time (`maxSurge: 1`, `maxUnavailable: 1`, the infrastructure repo's `base/agent-gateway.yaml`), so that window lasts from the first new pod's readiness until the last old pod stops taking requests; Django still refuses an agent's order from a cart whose earlier order awaits its online payment (django-api #693). From the move after `cs:v7` on, pods of both releases hold such a cart.

So before the deploy, list the sessions whose order exists or may. The gateway's Redis is its own instance, `redis-gateway`, database **0** (`REDIS_URL` in the infrastructure repo's `base/agent-gateway.yaml`), and its container already has `REDISCLI_AUTH` set. Run this from your machine: it runs the check in that container's shell (`sh -s`), so a failure ends the check and not your shell. It prints how many sessions it read (a session's lock and its placed-order record are not sessions and are not counted; the carts' records are under `ag:*:checkoutcart:*`, outside the scan), then the ones that matter:

```bash
NS=grooveshop # grooveshop-staging for staging
kubectl --context grooveshop -n "$NS" exec -i redis-gateway-0 -c redis-gateway -- sh -s <<'CHECK'
R="redis-cli -e -n 0"
$R ping | grep -qx PONG || { echo "Redis did not answer PONG: nothing was checked"; exit 1; }
all=$($R --scan --pattern 'ag:*:cs:*') || { echo "the scan failed: nothing was checked"; exit 1; }
keys=$(printf '%s\n' "$all" | grep -Ev ':(lock|order)$')
echo "sessions scanned: $(printf '%s\n' "$keys" | grep -c .)"
found=0
while read -r key; do
  [ -n "$key" ] || continue
  value=$($R GET "$key") || { echo "GET $key failed: the list is incomplete, do not deploy on it"; exit 1; }
  [ -n "$value" ] || continue # it ended between the scan and the read
  status=$(printf '%s' "$value" | grep -o '"status":"[a-z_]*"' | head -1 | cut -d'"' -f4)
  [ -n "$status" ] || { echo "$key holds no status: the list is incomplete, do not deploy on it"; exit 1; }
  case $status in
    complete_in_progress | requires_escalation) echo "$status $key"; found=1 ;;
  esac
done <<EOF
$keys
EOF
[ "$found" = 1 ] || echo "none being placed or awaiting payment"
CHECK
```

`-e` makes `redis-cli` exit non-zero when Redis refuses a command, which is how the check tells a failed read from an empty answer. "none being placed or awaiting payment" is printed only when every read succeeded. "sessions scanned: 0" with agent checkouts in use means the wrong Redis or database, not an empty one.

- **None:** deploy.
- **Some:** wait, or deploy and accept that those platforms are told an order may exist and to check for it. A session awaiting payment leaves the list when its payment lands (it becomes `completed`), when its order is canceled and an event or a read says so (from the `cs:v5` release on), or 24 hours after its order was placed, one whose order call went unanswered 24 hours after that was stored, and one only marked in progress 30 minutes after the mark. (On pods older than the #84 release, each left 30 minutes after its last save.)

**A rollback** to the release before a key move (never further back than the one with gateway #138) is the same split with the roles swapped:

- Every session the newer release stored, under its own key, is invisible to the old pods and gets the missing-session answer. It is still in Redis until it ends, with the placed-order record beside it (`…:order`), and is found again if this release is deployed again before then.
- A session stored under the old key before the upgrade is visible again if it has not ended.
- A payment event for a session under the newer key that reaches an old pod is answered 204 and is not sent again: after a new deploy that session still reads `requires_escalation` until it ends, while `get_order` shows the paid order.
- What the newer release adds is gone until it is deployed again. Back from `v2`: the old pods store a session without checking what it holds, and two completions of one checkout can again place two orders. Back from `v3`: no checkout holds its cart, and two checkouts on one cart can again each place an order. Back from `v4`: a checkout is no longer bound to the platform that signed its creation, and `get_order` reads no platform's orders.

So run the check above before a rollback too, and do not roll back further than the release with #138: older pods answer a session they cannot see with only "start a new one".

## 10. A checkout stuck in progress

**What it is.** A session whose stored status stays `complete_in_progress`. A completion marks the session before it asks Django for the order, and stores the outcome after. The session stays marked when that second write never happens:

| What happened | Is there an order? | How long the session stays |
|---|---|---|
| The mark was stored, Redis failed its reply and the second attempt's, and the read that looks for the mark failed as well, or did not answer before the completion's budget ended. No order was sent | No | 30 minutes after the mark |
| Django refused the order, and Redis failed the save that makes the session ready again, twice | No | 30 minutes after the mark |
| The order was placed, and Redis failed the save that records it, twice | Yes. It is recorded beside the session (`…:order`) unless that write failed as well, which the log says; with the record, every call on the session names the order. The next order event records it on the session either way (it names its order, and Django supplies the rest), which is then kept 24 hours from the placement | 30 minutes after the mark, unless an event comes first |
| The order call went unanswered, and that was stored (`"unconfirmed":true`) | Unknown | 24 hours |
| The pod died between the mark and Django's answer | Unknown | 30 minutes after the mark |

Callers are told "in progress" for three upstream timeouts after the mark. After that, or from the first call for a session stored `"unconfirmed":true`, **a complete resumes the session** (gateway #83): the gateway sends the order again with the checkout's id, and Django, which keeps one order per agent checkout (Django #759), answers the order the checkout placed, or places it if the first call never reached it, or refuses it. The checkout then ends as any completion does, with at most one order. Update and cancel, and a complete Django still does not answer, are told "the store has not confirmed whether this checkout's order was placed" and to complete again shortly, or the order when it is known. Within one completion the gateway already sends an unanswered order again, up to `ORDER_CREATE_ATTEMPTS` times `ORDER_CREATE_RETRY_DELAY` apart, inside the order's one `UPSTREAM_TIMEOUT`, so a stuck session is one whose Django was down or slow for that long. Once the session has ended its callers get the missing-session answer, which says an order may exist and how to check.

**Nothing needs doing**: the platform's next complete settles it. Look only when a buyer or a platform is waiting on one whose completes keep failing (Django down or throttling the cart's order creations, logged at the complete). Then:

1. Find it. In the check of section 9, the lines that start `complete_in_progress` are these sessions. For one of them, in a shell in the same container (`kubectl --context grooveshop -n <namespace> exec -it redis-gateway-0 -c redis-gateway -- sh`), with `R="redis-cli -e -n 0"`:

   ```bash
   $R GET "$key"            # status, updatedAt, cartId, the buyer's email, "unconfirmed", orderUuid
   $R EXISTS "$key:order"   # 1: the order was placed, and GET "$key:order" names it
   $R PTTL "$key"           # how long the session still lives, in ms
   ```

   The gateway's completion has ended once `updatedAt` is more than three times `UPSTREAM_TIMEOUT` old. Django's order request may still be running: the gateway's timeout ends its wait, not Django's work.

2. Decide whether an order exists. Only the checkout's id names an order as this checkout's: an order found by the buyer's email, the products or the total may be another of the buyer's orders, and is not this checkout's on that alone.
   - `$key:order` exists, or the session has an `orderUuid`: it does. Leave the session; give the platform or the buyer the order.
   - Otherwise ask Django for the order that names the checkout (`agentCheckout`, on every order a `cs:v7` or later session sends; Django #759). `<schema>` and `<session id>` are the key's second and last parts:

     ```bash
     kubectl --context grooveshop -n <namespace> exec deploy/backend -c backend -- \
       .venv/bin/python manage.py shell -c "
     from uuid import UUID
     from django_tenants.utils import schema_context
     from order.services import OrderService
     with schema_context('<schema>'):
         o = OrderService.order_for_agent_checkout(UUID('<session id>'))
         print(o.uuid if o else 'none')
     "
     ```

     An order printed is this checkout's, a deleted one included. `none` means no order only once Django's order request has ended (step 1): one still running can commit its order after.
   - Search the gateway's logs for the session id (`session`) too. "checkout: session save failed" and "checkout: order index write failed" each name the `order` when one exists. "checkout: the store did not tie the order to its checkout" names an order Django took without the checkout's id, which the lookup above cannot find: that order is this checkout's.
   - Anything else (an admin search by email, products or total that finds a candidate, or the buyer's confirmation email) leaves the outcome unknown: keep the session (step 3).

3. Clear it **only when you have confirmed that no order exists**, and only while no complete can resume it. A complete of a session stuck in progress sends its order again (section 10's first paragraph), so a confirmation taken while a complete may arrive does not justify the delete: hold the session first, confirm under the hold, and delete under it.
   1. Hold it with the session's own lock, which every update, complete and cancel takes before it reads the session (a complete that finds it held is told the session is busy, and retries). The hold lasts 15 minutes:

      ```bash
      hold="inflight:operator-$(date +%s)"
      $R SET "$key:lock" "$hold" NX PX 900000   # OK: held. (nil): a request holds it; try again shortly
      ```

      If it never answers `OK`, keep the session.
   2. Read the session again under the hold: `$R GET "$key"`. It must still be `complete_in_progress` with no `orderUuid`, `$R EXISTS "$key:order"` must be `0`, and its `updatedAt` must be more than three times `UPSTREAM_TIMEOUT` old (a resume that ran before the hold moved it, and may still be running). Note its `revision`, `id` and `cartId`.
   3. Confirm, now, that no order exists (step 2): no order was sent, or the order request ended in Django and the lookup prints `none`. A confirmation taken before the hold does not count.
   4. Delete the session and its cart's record, only while the hold is still yours and the session is still the one you read. The cart's record (`ag:{schema}:checkoutcart:{cartId}`) names the session; left behind, it would keep the cart from every new checkout until it ended (a record whose session the gateway does not read holds the cart, section 9):

      ```bash
      $R EVAL "
      if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 'hold lost' end
      local s = redis.call('GET', KEYS[2])
      if not s or cjson.decode(s).revision ~= ARGV[3] then return 'session changed' end
      redis.call('DEL', KEYS[2])
      if redis.call('GET', KEYS[3]) == ARGV[2] then redis.call('DEL', KEYS[3]) end
      redis.call('DEL', KEYS[1])
      return 'deleted'
      " 3 "$key:lock" "$key" "ag:<schema>:checkoutcart:<cartId>" "$hold" "<id>" "<revision>"
      ```

      `deleted`: the next call on that checkout id gets the missing-session answer, and the platform starts a new checkout. `hold lost` (it lapsed) or `session changed`: nothing was deleted; start again from step 3.1.
   5. If you stop before step 4, release the hold only while it is yours: `$R EVAL "if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0" 1 "$key:lock" "$hold"`. Left, it lapses after its 15 minutes.

   If the outcome stays unknown, keep the session and start no new checkout. A session with `"unconfirmed":true` is otherwise kept the full 24 hours.

   When an order exists, do not delete the session to "unblock" the platform: a new checkout for the same cart would be a second order. The order is the answer, and a complete of the session, once Django answers again, records it.

## 11. Sealing the stores' signing keys

**Applies to:** the release that seals each store's UCP signing key in Redis, and every environment that ran an earlier one.

Each store's ES256 key signs its order webhooks; its public half is in the store's `/.well-known/ucp`, where platforms verify the webhooks. Releases before this one held the private key unsealed (`ag:{schema}:ucp:signing_key:es256`, base64), readable by every service sharing that Redis. This release holds it in `ag:{schema}:ucp:signing_key:es256:aes256gcm`, sealed with AES-256-GCM under `SIGNING_KEY_ENCRYPTION_KEY` and bound to its store. It seals the same key, so the key id and the published key do not change: no platform sees a rotation.

**How the move runs.** A pod of this release reads only the sealed record. The first time it needs a store's key and finds only the unsealed one, it seals that key; it never mints another over it. For a store with no key at all it mints one, sealed, and writes a sentinel into the unsealed record: a pod of the earlier release cannot decode the sentinel (its profile answers 503 for that store) and cannot mint a second key over it. The unsealed records stay until step 3, because pods of the earlier release still read them during the rollout. During the rollout itself, an old pod's dispatcher drops the order webhooks of a store whose first signing key a new pod minted: it reads the sentinel, fails three times and acknowledges them. Only a store that had no key before the rollout is affected; every store that has served its UCP profile already has one.

1. **Before the release, in each environment:** generate a key-encryption key with `openssl rand -base64 32`, one per environment, and save it in Bitwarden (personal vault, GrooveShop) before anything else. Add it as `SIGNING_KEY_ENCRYPTION_KEY` to the SealedSecret `agent-gateway-secrets` in the infrastructure repository (`overlays/staging/` and `overlays/production/sealed-agent-gateway-secrets.yaml`, `kubeseal --merge-into`). The Deployment takes that secret whole (`envFrom`), so nothing else changes. Pods of the earlier release ignore it. A pod of this release without it refuses to start, so a missing value stalls the rollout on the old pods and breaks nothing. So does a wrong one: a pod opens the sealed keys Redis holds as it starts, stays not ready until it has, and exits (`SIGNING_KEY_ENCRYPTION_KEY opens none of the stored signing keys`) when there is at least one and none opens, for example another environment's key. While Redis does not answer it waits, logged `signing keys not checked yet; retrying`, and is simply not ready.
2. **Deploy the release.**
3. **Once no pod of the earlier release is left,** seal what is left and delete the unsealed records:

   ```bash
   kubectl --context grooveshop -n grooveshop-staging exec deploy/agent-gateway -c agent-gateway -- /gateway seal-signing-keys
   kubectl --context grooveshop -n grooveshop exec deploy/agent-gateway -c agent-gateway -- /gateway seal-signing-keys
   ```

   It seals every store's key still unsealed, deletes every unsealed record whose key the sealed record holds and every sentinel, logs one `signing keys sealed` line (`sealed`, `already_sealed`, `unsealed_deleted`), and exits non-zero if it left any store alone (`signing key left unsealed`, with the reason). Run it a second time right after: its line then lists no store under `sealed` or `unsealed_deleted`, which proves nothing unsealed is left. Later runs may list stores under `unsealed_deleted`: a store first keyed after step 3 still gets a sentinel, which expires by itself 7 days after it was written, so nothing needs deleting by hand.
4. **A store it left alone** holds one key unsealed and another sealed. That takes a record deleted by hand while an old pod ran. The new pods publish the sealed key; delete the unsealed record by hand once you know which key platforms were last shown.

**Never change `SIGNING_KEY_ENCRYPTION_KEY` in place.** A key sealed under another one does not open: a pod started with it exits once Redis holds keys and none opens (`AwaitKEK`), and is never ready, so the gateway serves nothing. Django's outbox keeps the order events meanwhile, until the key is right or their attempts run out. Changing it needs every record sealed again under the new key, which no command does yet.

**A rollback** to the earlier release:

- **Before step 3:** every store that had a key before this release keeps it (the unsealed record still holds it). A store whose first key this release minted holds the sentinel for 7 days: until it expires, the earlier release answers 503 on its profile and drops its webhooks after three attempts; after, or once it is deleted, the earlier release mints a new key for the store, a rotation its platforms see.
- **After step 3:** no unsealed record is left, so the earlier release mints a new key for every store, a rotation every platform sees. Do not roll back past this release once step 3 has run.

The release after every environment has run step 3 deletes the sealing on first use, the sentinel, and the command (`internal/ucp/keys_unsealed.go`, `cmd/gateway/sealkeys.go`). Sentinels written until then expire within 7 days, so that release needs no cleanup of its own.

**Eviction does not happen.** The signing keys carry no TTL, and the gateway's Redis is its own instance with `maxmemory-policy noeviction` and AOF persistence, so nothing evicts them; a full Redis refuses writes instead.

**Giving a store a new key on purpose** (its key leaked). Only once step 3 has run in that environment and no pod of the earlier release is left: while the store's unsealed record holds its key, a pod that finds no sealed record seals that same key again, so deleting the sealed record alone changes nothing. Delete both records, then restart the pods. `redis-cli` runs in the gateway's Redis container, as in section 9 (database 0, `REDISCLI_AUTH` already set):

```bash
NS=grooveshop # grooveshop-staging for staging
SCHEMA='<schema>'
kubectl --context grooveshop -n "$NS" exec redis-gateway-0 -c redis-gateway -- \
  redis-cli -e -n 0 DEL "ag:$SCHEMA:ucp:signing_key:es256:aes256gcm" "ag:$SCHEMA:ucp:signing_key:es256"
kubectl --context grooveshop -n "$NS" rollout restart deploy/agent-gateway
```

Each pod holds the key it read in memory until it restarts, and the first pod to need the key after the restart mints the new one. Platforms see a rotation: the profile publishes only the new key, so a webhook signed with the old one before the restart no longer verifies.

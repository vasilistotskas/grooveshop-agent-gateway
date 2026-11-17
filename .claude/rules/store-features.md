---
paths:
  - "internal/mcpsrv/server.go"
  - "internal/mcpsrv/discovery.go"
  - "internal/mcpsrv/shipping.go"
  - "internal/mcpsrv/policies.go"
  - "internal/mcpsrv/account*.go"
  - "internal/django/tenant_config.go"
  - "internal/tenant/**"
  - "internal/chat/bridge.go"
  - "internal/checkout/discounts.go"
  - "internal/ucp/**"
  - "internal/acp/**"
  - "internal/feeds/**"
  - "internal/adminmcp/**"
---

# Store features and store facts

Loads when working on what a store serves: its feature switches, and the facts tools read from it. The code wins over this page.

- **A store feature switched off is off on every surface.** Django
  folds each into one boolean, the plan flag AND the store setting, under
  `features` on `tenant/resolve` (`TenantConfig.Features`); read
  nothing else. A tool serving a feature is registered with `onlyWith`
  (`internal/mcpsrv/server.go`): the catalog tools need `catalogue`;
  the cart, shipping, payment-method, checkout-link and five canonical
  checkout tools need `cart` (which Django folds the catalogue into);
  `get_product_reviews`, `subscribe_product_alert` and `my_favourites`
  need `catalogue` and `productReviews` / `productAlerts` /
  `favourites`; `my_loyalty_points` `loyalty`. Every account tool
  (`account`) also needs `customerAccounts`, so `my_orders` too; Django
  folds it into reviews, favourites and loyalty as well. The other order
  tools and `get_store_policies` stay open (return terms bind orders
  already placed). The UCP profile and `/acp/*` need `cart` (`CheckoutOn`), the
  feeds `catalogue` (`ProductFeedsOn`). The per-store server (cached per feature set) neither
  lists nor serves it, discovery does not advertise it, and the chat
  bridge, which shares one server built with `EveryFeature`, filters
  through `mcpsrv.Offered`. Which surface serves a tool is a trait on
  its definition too (`tool.on`): `chatOnly` (the coupon tools),
  `mcpOnly` (the six UCP tools, whose input requires `meta`, and
  `create_cart`) and `account`. With reviews off no rating or review count
  leaves the catalog tools either (ratings are on Django's 1–10 scale).
  With promotions off `checkout.ApplyDiscountCodes` refuses every code
  as `discount_code_invalid` without applying it (Django answers 404
  with the plan flag off, which would fail the request) and clears the
  cart's old coupon best effort, ACP sessions do not declare the
  discount extension, and the UCP profile does not advertise
  `dev.ucp.shopping.discount` (`ucp.BusinessCapabilities`), so a UCP
  code is ignored as a member of an extension that was not negotiated.
- **Store facts come from the store, never a default.**
  `find_pickup_points` searches only the pickup carriers the store's
  `/shipping/options` list as `pickup_point` for the country, among
  the networks it can search (ACS, BOX NOW). The cart is judged
  (`FitsCart`) only in the chat, through the shopper's cart token:
  the tool takes no cart, so on `/mcp` no carrier is left out for
  the cart's size;
  a store without one gets an answer saying so, not an error. BOX NOW's
  `lockers/nearest` requires city, street AND postal code, so it is
  called only with all three and the tool tells the model to ask for
  the rest. `get_store_policies` reads `RETURN_WINDOW_DAYS` and
  `RETURN_FEES` from `/settings/public` (Django's `PUBLIC_SETTING_KEYS`;
  `0` / `""` are "not stated" and are left out, a missing or malformed
  value fails the call) and links `Links.ReturnPolicy` only while
  `/content-page/return-policy` is published (a 404 means it is not).

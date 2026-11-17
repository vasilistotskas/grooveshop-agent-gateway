---
paths:
  - "internal/storefront/**"
  - "internal/mcpsrv/helpers.go"
  - "internal/feeds/**"
  - "internal/acp/render.go"
  - "internal/ucp/**"
---

# Storefront links (`internal/storefront`)

Loads when working on a link into the Nuxt storefront. The paths are a cross-repo contract with the storefront. The code wins over this page.

- `internal/storefront` — every link into the Nuxt storefront (product,
  cart claim, order success, legal pages, OAuth metadata). The paths are
  a cross-repo contract with the Nuxt `app/pages` and `server/routes`
  trees; build URLs here, never with a `"https://" + domain + path`
  literal in a renderer. Page links are in a language: `storefront.Site`
  (built once from `STOREFRONT_DEFAULT_LOCALE`) hands out
  `Links(domain, lang)`, which leave the build default unprefixed and
  put any other language under `/<code>` (@nuxtjs/i18n
  `prefix_except_default`). The cart claim is a Nitro server route, not
  a page, so it takes no prefix: it carries a `locale=<code>` query instead,
  only for a language other than the build default, and the storefront
  redirects to the cart in it. A tool builds its links in the turn's
  language (`mcpsrv` `linksFor`: `django.Language`), so a chat turn on
  `/en` links `/en/...` and `/mcp` links the store's default; ACP, the
  UCP order webhooks, the Server Card and the feeds link the store's
  default.

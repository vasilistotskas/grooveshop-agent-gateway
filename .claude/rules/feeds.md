---
paths:
  - "internal/feeds/**"
  - "internal/httphdr/**"
---

# Product feeds (`internal/feeds`)

Loads when working on the feeds or the header parsing they share with the Server Card. The code wins over this page.

- `internal/feeds` — one catalog sweep renders every feed, cached in
  Redis (`ag:{schema}:feed:{format}` plus `:meta`) between
  `FEED_FRESH_TTL` and `FEED_STALE_TTL`. Three documents (`formats`):
  Google's RSS (`google`), the RSS Meta and TikTok share (`rss`), and
  ACP's JSON (`acp`). The two RSS documents differ only in `g:brand` for
  a product without a brand of its own: the store's name for Meta and
  TikTok, which require one, and none for Google, which asks for the
  manufacturer's (`newGoogleWriter`, `newMetaTikTokWriter`); no
  `g:identifier_exists` is written. Each document is written an
  item at a time into its gzip stream (`docStream`), which hashes and
  counts it for the ETag and `Meta.Size`: no feed is ever whole and
  uncompressed in memory, so a sweep costs the fetch window plus the
  gzip output, about a tenth of the documents (`memory_measure_test.go`,
  build tag `feedmem`, measures it). A failed generation is logged
  once with the request that started it, counted
  (`gateway_feed_generation_failures_total`) and marked
  (`ag:{schema}:feed:failed`, `failureBackoff`): until the mark expires
  or an invalidation drops it, a stale feed is served without a refresh
  and a missing one answers 503 without a sweep. A generation whose feed
  is gone again before it is read back is marked too; a read-back that
  failed is not (the feed was written): the request gets that error,
  its own context's when the crawler left, with no log line and no
  mark. A failure an invalidation overtook (its epoch moved) is not
  marked, and one that failed before reading the epoch always is. A revalidation reads the meta key
  and only TOUCHes the blob (the two keys can be lost apart, by a
  flush or a restore, and TOUCH, unlike EXISTS, marks the blob read
  with the meta key): an
  entry without its blob is a miss and regenerates, and so is one whose
  blob is not whole: every body read checks the gzip header and the
  trailer's ISIZE against the stored size, in constant time, with no
  decoding. The gzip coding writes the blob as stored; only an identity
  GET decodes it, streamed under its declared length and never past it,
  with its last `holdBack` bytes written only once gzip has verified the
  CRC and the length. A decode that fails cuts the response short of
  `Content-Length` (net/http closes the connection), and the blob is
  logged and dropped (`dropBlob`, as the header check does), so the next
  request regenerates it. The drop is a compare-and-delete on the meta
  record read with the blob, so a blob a generation wrote since stays. One weak ETag covers the gzip and
  the identity body; a 200 (and a HEAD) declares `Content-Length` in
  either coding, a 304 carries only `ETag`, `Cache-Control` and `Vary`,
  and an error answer none of the three. The sweep is
  detached from the request (`context.WithoutCancel`) and names no
  caller to Django (`django.WithoutCaller`): it is the store's work, not
  the crawler's. It keeps the request's id and store, so every line it
  logs, the Django client's included, names both
  (`TestASweepsDjangoLinesNameTheStoreAndRequest`). Field limits are the strictest of Google, Meta and
  TikTok, named at the top of `rss.go`. A description drops what the
  storefront does not show: raw-text elements and any element carrying
  `hidden` (`plainText`). `internal/httphdr` parses
  `If-None-Match` and `Accept-Encoding` for the feeds and the Server
  Card alike.

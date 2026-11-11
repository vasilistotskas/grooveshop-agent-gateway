package feeds

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/media"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Kinds served under /feeds/.
const (
	KindGoogle = "google"
	KindMeta   = "meta"
	KindTikTok = "tiktok"
	KindACP    = "acp"
)

// The three RSS kinds serve one document (see rss.go), so it is
// rendered, compressed and cached once; a kind only picks its format.
const (
	formatRSS = "rss"
	formatACP = "acp"
)

var formats = []string{formatRSS, formatACP}

func formatOf(kind string) string {
	if kind == KindACP {
		return formatACP
	}
	return formatRSS
}

// generationSlots bounds concurrent catalog sweeps per pod (memory
// guard); the semaphore is shared by every tenant.
var generationSlots = make(chan struct{}, 2)

// generationTimeout bounds one sweep, including its wait for a slot.
const generationTimeout = 5 * time.Minute

// storeAttempts bounds the optimistic cache write against concurrent
// invalidations; each retry means another invalidation landed mid-write.
const storeAttempts = 3

// failureBackoff is how long a failed generation holds off the next one
// for its store, across every pod. Without it, every request between
// FEED_FRESH_TTL and FEED_STALE_TTL (18h by default) or on a missing feed
// starts another full sweep of the catalog while the failure lasts. A
// stale feed is still served meanwhile; a missing one answers 503 for at
// most this long, and an invalidation lifts it at once.
const failureBackoff = time.Minute

// failureMarkTimeout bounds the retries of the failure mark's write, so a
// Redis outage does not hold the store's singleflight key behind the
// client's retry backoff. It does not bound one command's socket I/O:
// go-redis applies a context deadline there only with
// ContextTimeoutEnabled, which the gateway's client does not set, so a
// hung command still returns after the client's read timeout.
const failureMarkTimeout = 2 * time.Second

// noEpoch stands for an epoch a generation never read: Incr starts the
// real ones at 1, and an absent key reads 0.
const noEpoch = -1

// errRecentlyFailed answers a missing feed while failureBackoff runs.
var errRecentlyFailed = errors.New(
	"feeds: the last generation failed; waiting before the next")

type Meta struct {
	ETag string `json:"etag"`
	// GeneratedAt is when the sweep that produced the feed STARTED — the
	// catalog is as of then. Zero marks a feed an invalidation overtook:
	// servable, but stale on arrival.
	GeneratedAt time.Time `json:"generatedAt"`
	Size        int       `json:"size"`
	// Generator is the gateway build that rendered the feed. A feed from
	// another build is not served: the cache outlives a release by up to
	// FEED_STALE_TTL, and a release that changes what a feed says (new
	// attributes, a fixed URL) reached Google, Meta and TikTok only once
	// that ran out. During a rollout the old and the new pods may each
	// regenerate once; after it, one build writes and reads.
	Generator string `json:"generator"`
}

type Service struct {
	dj      *django.Client
	rdb     *redis.Client
	log     *slog.Logger
	metrics *obs.Metrics
	// imageTpl expands {assets_host}/{schema}/{path}; assetsHost is the
	// PLATFORM media origin used for tenants that have not opted into
	// white-label asset URLs (the documented default).
	imageTpl   string
	assetsHost string
	freshTTL   time.Duration
	staleTTL   time.Duration
	generator  string
	site       storefront.Site
	sf         singleflight.Group
}

func NewService(
	dj *django.Client, rdb *redis.Client, log *slog.Logger,
	metrics *obs.Metrics,
	imageTpl, assetsHost string, freshTTL, staleTTL time.Duration,
	generator string, site storefront.Site,
) *Service {
	return &Service{
		dj: dj, rdb: rdb, log: log, metrics: metrics,
		imageTpl: imageTpl, assetsHost: assetsHost,
		freshTTL: freshTTL, staleTTL: staleTTL,
		generator: generator, site: site,
	}
}

func dataKey(schema, format string) string {
	return "ag:" + schema + ":feed:" + format
}

func metaKey(schema, format string) string {
	return dataKey(schema, format) + ":meta"
}

// epochKey counts invalidations, so a sweep can tell whether one landed
// while it was reading the catalog.
func epochKey(schema string) string {
	return "ag:" + schema + ":feed:epoch"
}

// failedKey marks a store whose last generation failed, for
// failureBackoff.
func failedKey(schema string) string {
	return "ag:" + schema + ":feed:failed"
}

// Meta returns the metadata of the feed the store serves, without its
// body, so a revalidation that matches costs no blob read. An entry
// counts only while its blob is there too (Redis evicts the keys one by
// one), checked in the same round trip. A fresh entry is returned as it
// is; a stale one too, while a background refresh runs; a miss waits for
// a generation. After a failed generation, failureBackoff holds off the
// next: a stale entry is returned without a refresh and a miss fails at
// once. One generation renders every kind.
func (s *Service) Meta(
	ctx context.Context, t *tenant.Tenant, kind string,
) (Meta, error) {
	format := formatOf(kind)
	// An unreadable cache is a miss here: the generation that follows
	// needs Redis too, and is marked if it cannot reach it.
	meta, ok, failed, _ := s.cachedMeta(ctx, t.SchemaName, format)
	if ok {
		if !failed && time.Since(meta.GeneratedAt) >= s.freshTTL {
			s.refresh(ctx, t)
		}
		return meta, nil
	}
	epoch, err := s.await(ctx, t, failed)
	if err != nil {
		return Meta{}, err
	}
	meta, ok, _, err = s.cachedMeta(ctx, t.SchemaName, format)
	if err != nil {
		return Meta{}, err
	}
	if !ok {
		return Meta{}, s.notCached(ctx, t.SchemaName, epoch)
	}
	return meta, nil
}

// Body returns the gzipped feed with the metadata stored beside it, both
// read in one step; the blob decodes to exactly meta.Size bytes. A blob
// gone since Meta (evicted, invalidated, rewritten by another build), or
// one that does not decode to its size, is a miss like any other: it
// waits for a generation, under the same failure backoff, and is read
// again.
func (s *Service) Body(
	ctx context.Context, t *tenant.Tenant, kind string,
) ([]byte, Meta, error) {
	format := formatOf(kind)
	gz, meta, ok, failed, _ := s.fromCache(ctx, t.SchemaName, format)
	if ok {
		return gz, meta, nil
	}
	epoch, err := s.await(ctx, t, failed)
	if err != nil {
		return nil, Meta{}, err
	}
	gz, meta, ok, _, err = s.fromCache(ctx, t.SchemaName, format)
	if err != nil {
		return nil, Meta{}, err
	}
	if !ok {
		return nil, Meta{}, s.notCached(ctx, t.SchemaName, epoch)
	}
	return gz, meta, nil
}

var errGenerationNotCached = errors.New("feeds: generation produced no cache")

// await joins the store's generation for a missing feed, or fails at
// once while a failed one's backoff runs. It returns the epoch the
// generation read.
func (s *Service) await(
	ctx context.Context, t *tenant.Tenant, failed bool,
) (int64, error) {
	if failed {
		return noEpoch, errRecentlyFailed
	}
	select {
	case res := <-s.refresh(ctx, t):
		epoch, _ := res.Val.(int64)
		return epoch, res.Err
	case <-ctx.Done():
		return noEpoch, ctx.Err()
	}
}

// notCached handles a generation that succeeded but whose feed is not
// there when read back (evicted again at once, under memory pressure), or
// does not decode (fromCache logs why): without a mark, every request
// would run another full sweep and still get 503. An invalidation since the generation's epoch lifts it, as for
// a failure. A read-back that failed is not this: the generation wrote
// the feed, so the failure says nothing about it. The request gets that
// error (its own context's, when the caller left) and nothing is marked,
// so the next request reads the feed.
func (s *Service) notCached(
	ctx context.Context, schema string, epoch int64,
) error {
	s.log.ErrorContext(ctx, "feed generated but gone from the cache")
	s.mark(ctx, schema, epoch)
	return errGenerationNotCached
}

// refresh starts (or joins) the tenant's generation. The sweep is
// detached from the request that started it: context.WithoutCancel keeps
// the request's id and tenant for the log lines and Django's correlation
// id, and django.WithoutCaller drops the visitor, whose throttle and
// identity a sweep for the store must not spend. On a large catalog it
// can outlast a crawler's timeout, and tying it to the first caller would
// cancel it for every waiter and restart it from zero on the next
// request, a feed that never lands. An abandoned sweep still fills the
// cache the next crawler reads, and singleflight keeps it to one per
// tenant. A panic is logged and returned as an error (obs.Recover): here
// for the whole generation on the singleflight goroutine, and in
// fetchAllProducts for pages 2..N, fetched on errgroup goroutines. A
// failure is logged, counted and marked here, whichever caller waits for
// it or none.
func (s *Service) refresh(
	ctx context.Context, t *tenant.Tenant,
) <-chan singleflight.Result {
	ctx = django.WithoutCaller(context.WithoutCancel(ctx))
	return s.sf.DoChan(t.SchemaName, func() (_ any, err error) {
		gctx, cancel := context.WithTimeout(ctx, generationTimeout)
		defer cancel()
		epoch := int64(noEpoch)
		defer func() { s.failed(ctx, t.SchemaName, epoch, err) }()
		defer obs.Recover(gctx, s.log, "feeds: generation", &err)

		select {
		case generationSlots <- struct{}{}:
		case <-gctx.Done():
			return nil, gctx.Err()
		}
		defer func() { <-generationSlots }()
		// The epoch is read before the first catalog page, so an
		// invalidation at any point during the sweep is seen at write
		// time, and a failure it overtook is not marked.
		read, err := readEpoch(gctx, s.rdb, t.SchemaName)
		if err != nil {
			return nil, fmt.Errorf("feeds: epoch: %w", err)
		}
		epoch = read
		return epoch, s.generate(gctx, t, epoch)
	})
}

// failed records a generation that ended in err, after obs.Recover has
// turned a panic into one, and marks it.
func (s *Service) failed(
	ctx context.Context, schema string, epoch int64, err error,
) {
	if err == nil {
		return
	}
	s.metrics.FeedFailures.Inc()
	s.log.ErrorContext(ctx, "feed generation failed",
		slog.String("error", err.Error()))
	s.mark(ctx, schema, epoch)
}

// mark sets the store's failure mark. With the epoch the generation read,
// only while it is current: an invalidation since may be the fix, and it
// has just emptied the feed. Without one (the generation failed before
// reading it: no slot, Redis unreadable), nothing can have overtaken it,
// and the mark is set as it is.
func (s *Service) mark(ctx context.Context, schema string, epoch int64) {
	mctx, cancel := context.WithTimeout(ctx, failureMarkTimeout)
	defer cancel()
	var err error
	if epoch == noEpoch {
		err = s.rdb.Set(mctx, failedKey(schema), 1, failureBackoff).Err()
	} else {
		err = s.rdb.Watch(mctx, func(tx *redis.Tx) error {
			current, err := readEpoch(mctx, tx, schema)
			if err != nil || current != epoch {
				return err
			}
			_, err = tx.TxPipelined(mctx, func(p redis.Pipeliner) error {
				p.Set(mctx, failedKey(schema), 1, failureBackoff)
				return nil
			})
			return err
		}, epochKey(schema))
	}
	if err != nil && !errors.Is(err, redis.TxFailedErr) {
		s.log.WarnContext(ctx, "feed failure not marked",
			slog.String("error", err.Error()))
	}
}

// cachedMeta reads the feed's metadata, whether its blob is there, and
// whether a generation failed within failureBackoff, in one round trip
// that reads no blob. TOUCH, not EXISTS: it refreshes the blob's LRU
// clock (EXISTS looks it up with LOOKUP_NOTOUCH), so revalidations that
// keep the meta key warm keep its blob warm too under allkeys-lru. A read
// that failed (Redis, or the caller's context) is an error, not a miss.
func (s *Service) cachedMeta(
	ctx context.Context, schema, format string,
) (meta Meta, ok, failed bool, err error) {
	var vals *redis.SliceCmd
	var blob *redis.IntCmd
	if _, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		vals = p.MGet(ctx, metaKey(schema, format), failedKey(schema))
		blob = p.Touch(ctx, dataKey(schema, format))
		return nil
	}); err != nil {
		return Meta{}, false, false, fmt.Errorf("feeds: cache read: %w", err)
	}
	if len(vals.Val()) != 2 {
		return Meta{}, false, false, nil
	}
	failed = vals.Val()[1] != nil
	meta, ok = s.decodeMeta(vals.Val()[0])
	return meta, ok && blob.Val() == 1, failed, nil
}

// fromCache reads the feed's blob and metadata, and whether a generation
// failed within failureBackoff, in one step. Like cachedMeta, a read that
// failed is an error, not a miss.
func (s *Service) fromCache(
	ctx context.Context, schema, format string,
) (gz []byte, meta Meta, ok, failed bool, err error) {
	vals, err := s.rdb.MGet(ctx, dataKey(schema, format),
		metaKey(schema, format), failedKey(schema)).Result()
	if err != nil {
		return nil, Meta{}, false, false,
			fmt.Errorf("feeds: cache read: %w", err)
	}
	if len(vals) != 3 {
		return nil, Meta{}, false, false, nil
	}
	failed = vals[2] != nil
	data, isString := vals[0].(string)
	meta, ok = s.decodeMeta(vals[1])
	if !isString || !ok {
		return nil, Meta{}, false, failed, nil
	}
	if err := decode([]byte(data), meta.Size, io.Discard); err != nil {
		s.log.ErrorContext(ctx, "cached feed does not decode",
			slog.String("error", err.Error()))
		return nil, Meta{}, false, failed, nil
	}
	return []byte(data), meta, true, failed, nil
}

// decode writes the feed gz holds to w. It fails unless gz is whole
// (gzip checks its CRC-32 and length once read to the end) and decodes
// to exactly size bytes; it never reads more than one byte past size.
func decode(gz []byte, size int, w io.Writer) error {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	n, err := io.CopyN(w, zr, int64(size)+1)
	switch {
	case err == nil:
		return fmt.Errorf("feeds: decodes past its %d bytes", size)
	case errors.Is(err, io.EOF) && n == int64(size):
		return nil
	case errors.Is(err, io.EOF):
		return fmt.Errorf("feeds: decodes to %d bytes, not %d", n, size)
	}
	return err
}

// decodeMeta reads a stored Meta; one from another build is a miss.
func (s *Service) decodeMeta(v any) (Meta, bool) {
	raw, ok := v.(string)
	if !ok {
		return Meta{}, false
	}
	var meta Meta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return Meta{}, false
	}
	if meta.Generator != s.generator {
		return Meta{}, false
	}
	return meta, true
}

// Invalidate drops every cached feed for one tenant so the next request
// regenerates from Django.
//
// Without this the only way out of a stale feed was to wait out
// FEED_FRESH_TTL (6h by default) or delete the keys by hand in Redis —
// so a merchant's price change, a new product, or a stock change took up
// to six hours to reach Google, Meta and TikTok. The cache survives pod
// restarts, so restarting the gateway did not help either.
//
// Bumping the epoch in the same transaction is what stops a sweep that
// read the catalog BEFORE the change from writing it back as fresh
// afterwards (see store).
//
// Returns the number of keys removed. Deliberately NOT a full
// regeneration: generating is a Django round trip per tenant, and the
// next feed request will do it anyway.
func (s *Service) Invalidate(ctx context.Context, schema string) (int64, error) {
	// The failure mark goes too: an invalidation follows a catalogue
	// change, which may be the fix.
	keys := make([]string, 0, len(formats)*2+1)
	keys = append(keys, failedKey(schema))
	for _, format := range formats {
		keys = append(keys, dataKey(schema, format), metaKey(schema, format))
	}
	var removed *redis.IntCmd
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Incr(ctx, epochKey(schema))
		// UNLINK, not DEL: a feed payload is a multi-megabyte gzip blob
		// and reclaiming it must not block the event loop.
		removed = p.Unlink(ctx, keys...)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed.Val(), nil
}

// generate sweeps the catalog once, rendering every format, and stores
// them gzipped with the stale TTL as the Redis expiry, unless an
// invalidation moved the store's epoch past epoch (see store).
func (s *Service) generate(
	ctx context.Context, t *tenant.Tenant, epoch int64,
) error {
	start := time.Now().UTC()
	fctx := &feedContext{
		StoreName:        storeName(t),
		Domain:           t.Domain,
		Links:            s.site.Links(t.Domain, t.DefaultLocale),
		AssetsHost:       media.Host(t.AssetsDomain, s.assetsHost),
		Schema:           t.SchemaName,
		Currency:         t.DefaultCurrency,
		Locale:           t.DefaultLocale,
		ImageURLTemplate: s.imageTpl,
		CategoryNames:    map[int64]string{},
	}
	cats, err := s.dj.ListAllCategories(ctx, t.Domain, t.DefaultLocale)
	if err != nil {
		return fmt.Errorf("feeds: categories: %w", err)
	}
	for _, c := range cats {
		fctx.CategoryNames[c.ID] = c.Translations[t.DefaultLocale].Name
	}

	rss := newRSSWriter(fctx)
	acp := newACPWriter()

	var listed, inactive, skipped, overCap int
	_, truncated, err := fetchAllProducts(
		ctx, s.dj, s.log, t.Domain, t.DefaultLocale,
		func(p django.ProductFeed) error {
			if !p.Active {
				inactive++
				return nil
			}
			item, err := newFeedItem(&p, fctx)
			if err != nil {
				return err
			}
			if item == nil {
				skipped++
				return nil
			}
			rss.Item(item)
			acp.Item(item)
			listed++
			if item.OverCap {
				overCap++
			}
			return nil
		})
	if err != nil {
		return err
	}
	if truncated {
		s.log.WarnContext(ctx,
			"feed catalog sweep hit the page cap — feed truncated",
			slog.Int("pages", maxPages))
	}

	acpBytes, err := acp.Bytes()
	if err != nil {
		return fmt.Errorf("feeds: acp encode: %w", err)
	}
	outputs := map[string][]byte{formatRSS: rss.Bytes(), formatACP: acpBytes}
	entries := make(map[string]cacheEntry, len(outputs))
	for format, raw := range outputs {
		gz, err := gzipBytes(raw)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		// Weak: the gzip and the identity body share it, and a strong
		// tag has to differ between codings (RFC 9110 8.8.3.3).
		entries[format] = cacheEntry{gz: gz, meta: Meta{
			ETag: `W/"` + hex.EncodeToString(sum[:16]) + `"`,
			Size: len(raw),
		}}
	}
	overtaken, err := s.store(ctx, t.SchemaName, epoch, start, entries)
	if err != nil {
		return err
	}

	s.log.InfoContext(ctx, "feeds generated",
		slog.Int("products", listed),
		slog.Int("inactive", inactive),
		slog.Int("skipped_no_name_image_or_price", skipped),
		slog.Int("products_over_a_cap", overCap),
		slog.Bool("overtaken_by_invalidation", overtaken),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	)
	return nil
}

type cacheEntry struct {
	gz   []byte
	meta Meta
}

// store writes the rendered formats unless an invalidation landed since
// epoch was read — WATCH makes that check and the write one atomic step.
// An overtaken sweep is still written, because it is the newest complete
// feed there is, but stamped stale so the next request regenerates
// instead of serving pre-change prices as fresh for FEED_FRESH_TTL.
func (s *Service) store(
	ctx context.Context, schema string, epoch int64, start time.Time,
	entries map[string]cacheEntry,
) (bool, error) {
	var overtaken bool
	write := func(tx *redis.Tx) error {
		current, err := readEpoch(ctx, tx, schema)
		if err != nil {
			return err
		}
		overtaken = current != epoch
		generatedAt := start
		if overtaken {
			generatedAt = time.Time{}
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			for format, e := range entries {
				meta := e.meta
				meta.GeneratedAt = generatedAt
				meta.Generator = s.generator
				rawMeta, err := json.Marshal(meta)
				if err != nil {
					return err
				}
				p.Set(ctx, dataKey(schema, format), e.gz, s.staleTTL)
				p.Set(ctx, metaKey(schema, format), rawMeta, s.staleTTL)
			}
			return nil
		})
		return err
	}
	for range storeAttempts {
		err := s.rdb.Watch(ctx, write, epochKey(schema))
		if !errors.Is(err, redis.TxFailedErr) {
			if err != nil {
				return false, fmt.Errorf("feeds: cache write: %w", err)
			}
			return overtaken, nil
		}
	}
	return false, errors.New("feeds: cache write kept losing to invalidations")
}

func readEpoch(
	ctx context.Context, c redis.Cmdable, schema string,
) (int64, error) {
	n, err := c.Get(ctx, epochKey(schema)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}

func storeName(t *tenant.Tenant) string {
	if t.StoreName != "" {
		return t.StoreName
	}
	return t.Name
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

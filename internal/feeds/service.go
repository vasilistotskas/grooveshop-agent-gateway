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
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/media"
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

type Meta struct {
	ETag string `json:"etag"`
	// GeneratedAt is when the sweep that produced the feed STARTED — the
	// catalog is as of then. Zero marks a feed an invalidation overtook:
	// servable, but stale on arrival.
	GeneratedAt time.Time `json:"generatedAt"`
	Size        int       `json:"size"`
}

type Service struct {
	dj  *django.Client
	rdb *redis.Client
	log *slog.Logger
	// imageTpl expands {assets_host}/{schema}/{path}; assetsHost is the
	// PLATFORM media origin used for tenants that have not opted into
	// white-label asset URLs (the documented default).
	imageTpl   string
	assetsHost string
	freshTTL   time.Duration
	staleTTL   time.Duration
	sf         singleflight.Group
}

func NewService(
	dj *django.Client, rdb *redis.Client, log *slog.Logger,
	imageTpl, assetsHost string, freshTTL, staleTTL time.Duration,
) *Service {
	return &Service{
		dj: dj, rdb: rdb, log: log,
		imageTpl: imageTpl, assetsHost: assetsHost,
		freshTTL: freshTTL, staleTTL: staleTTL,
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

// Get returns the gzipped feed. Fresh cache serves directly; a stale entry
// serves immediately while a background refresh runs; a miss waits for a
// generation. One generation pass renders every kind.
func (s *Service) Get(
	ctx context.Context, t *tenant.Tenant, kind string,
) ([]byte, Meta, error) {
	format := formatOf(kind)
	gz, meta, ok := s.fromCache(ctx, t.SchemaName, format)
	if ok {
		if time.Since(meta.GeneratedAt) >= s.freshTTL {
			s.refresh(t)
		}
		return gz, meta, nil
	}

	select {
	case res := <-s.refresh(t):
		if res.Err != nil {
			return nil, Meta{}, res.Err
		}
	case <-ctx.Done():
		return nil, Meta{}, ctx.Err()
	}
	gz, meta, ok = s.fromCache(ctx, t.SchemaName, format)
	if !ok {
		return nil, Meta{}, errors.New("feeds: generation produced no cache")
	}
	return gz, meta, nil
}

// refresh starts (or joins) the tenant's generation. The sweep is
// decoupled from any request: on a large catalog it can outlast a
// crawler's timeout, and tying it to the first caller would cancel it
// for every waiter and restart it from zero on the next request — a
// feed that never lands. An abandoned sweep still fills the cache the
// next crawler reads, and singleflight keeps it to one per tenant.
func (s *Service) refresh(t *tenant.Tenant) <-chan singleflight.Result {
	return s.sf.DoChan(t.SchemaName, func() (any, error) {
		ctx, cancel := context.WithTimeout(
			context.Background(), generationTimeout)
		defer cancel()
		return nil, s.generate(ctx, t)
	})
}

func (s *Service) fromCache(
	ctx context.Context, schema, format string,
) ([]byte, Meta, bool) {
	vals, err := s.rdb.MGet(ctx,
		dataKey(schema, format), metaKey(schema, format)).Result()
	if err != nil || len(vals) != 2 || vals[0] == nil || vals[1] == nil {
		return nil, Meta{}, false
	}
	data, ok1 := vals[0].(string)
	rawMeta, ok2 := vals[1].(string)
	if !ok1 || !ok2 {
		return nil, Meta{}, false
	}
	var meta Meta
	if err := json.Unmarshal([]byte(rawMeta), &meta); err != nil {
		return nil, Meta{}, false
	}
	return []byte(data), meta, true
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
	keys := make([]string, 0, len(formats)*2)
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
// them gzipped with the stale TTL as the Redis expiry.
func (s *Service) generate(ctx context.Context, t *tenant.Tenant) error {
	select {
	case generationSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-generationSlots }()

	// The epoch is read before the first catalog page so an invalidation
	// at any point during the sweep is visible at write time.
	epoch, err := readEpoch(ctx, s.rdb, t.SchemaName)
	if err != nil {
		return fmt.Errorf("feeds: epoch: %w", err)
	}
	start := time.Now().UTC()
	fctx := &feedContext{
		StoreName:        storeName(t),
		Domain:           t.Domain,
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

	var skipped int
	count, truncated, err := fetchAllProducts(
		ctx, s.dj, t.Domain, t.DefaultLocale,
		func(p django.Product) error {
			if !p.Active {
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
			return nil
		})
	if err != nil {
		return err
	}
	if truncated {
		s.log.Warn("feed catalog sweep hit the page cap — feed truncated",
			slog.String("tenant", t.SchemaName),
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
		entries[format] = cacheEntry{gz: gz, meta: Meta{
			ETag: `"` + hex.EncodeToString(sum[:16]) + `"`,
			Size: len(raw),
		}}
	}
	overtaken, err := s.store(ctx, t.SchemaName, epoch, start, entries)
	if err != nil {
		return err
	}

	s.log.Info("feeds generated",
		slog.String("tenant", t.SchemaName),
		slog.Int("products", count),
		slog.Int("skipped_no_name_or_image", skipped),
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

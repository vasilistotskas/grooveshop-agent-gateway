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

var kinds = []string{KindGoogle, KindMeta, KindTikTok, KindACP}

// generationSlots bounds concurrent catalog sweeps per pod (memory
// guard); the semaphore is shared by every tenant.
var generationSlots = make(chan struct{}, 2)

type Meta struct {
	ETag        string    `json:"etag"`
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

func dataKey(schema, kind string) string {
	return "ag:" + schema + ":feed:" + kind
}

func metaKey(schema, kind string) string {
	return dataKey(schema, kind) + ":meta"
}

// Get returns the gzipped feed. Fresh cache serves directly; a stale entry
// serves immediately while a background refresh runs; a miss generates
// synchronously. One generation pass renders every kind.
func (s *Service) Get(
	ctx context.Context, t *tenant.Tenant, kind string,
) ([]byte, Meta, error) {
	gz, meta, ok := s.fromCache(ctx, t.SchemaName, kind)
	if ok {
		age := time.Since(meta.GeneratedAt)
		if age < s.freshTTL {
			return gz, meta, nil
		}
		s.refreshAsync(t)
		return gz, meta, nil
	}

	if _, err, _ := s.sf.Do(t.SchemaName, func() (any, error) {
		return nil, s.generate(ctx, t)
	}); err != nil {
		return nil, Meta{}, err
	}
	gz, meta, ok = s.fromCache(ctx, t.SchemaName, kind)
	if !ok {
		return nil, Meta{}, errors.New("feeds: generation produced no cache")
	}
	return gz, meta, nil
}

func (s *Service) fromCache(
	ctx context.Context, schema, kind string,
) ([]byte, Meta, bool) {
	vals, err := s.rdb.MGet(ctx, dataKey(schema, kind), metaKey(schema, kind)).
		Result()
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
// Returns the number of keys removed. Deliberately NOT a full
// regeneration: generating is a Django round trip per tenant, and the
// next feed request will do it anyway (or serve stale-while-revalidate
// if a stale entry somehow remains).
func (s *Service) Invalidate(ctx context.Context, schema string) (int64, error) {
	keys := make([]string, 0, len(kinds)*2)
	for _, kind := range kinds {
		keys = append(keys, dataKey(schema, kind), metaKey(schema, kind))
	}
	// UNLINK, not DEL: a feed payload is a multi-megabyte gzip blob and
	// reclaiming it must not block the event loop.
	return s.rdb.Unlink(ctx, keys...).Result()
}

// refreshAsync regenerates in the background, decoupled from the request
// context; singleflight collapses concurrent refreshes per tenant.
func (s *Service) refreshAsync(t *tenant.Tenant) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_, _, _ = s.sf.Do(t.SchemaName, func() (any, error) {
			return nil, s.generate(ctx, t)
		})
	}()
}

// generate sweeps the catalog once, rendering all kinds, and stores them
// gzipped with the stale TTL as the Redis expiry.
func (s *Service) generate(ctx context.Context, t *tenant.Tenant) error {
	// Wait for a slot OR for the caller to go away. A bare send ignored
	// cancellation: when a client gave up on a cold feed, the goroutine
	// kept queuing, eventually took a slot, and ran a full catalog sweep
	// against Django for a request nobody was listening to. A burst of
	// cold feed requests across tenants therefore queued an unbounded
	// pile of sweeps behind a two-slot semaphore that is shared by every
	// tenant.
	select {
	case generationSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-generationSlots }()

	start := time.Now()
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

	rss := map[string]*rssWriter{
		KindGoogle: newRSSWriter(fctx),
		KindMeta:   newRSSWriter(fctx),
		KindTikTok: newRSSWriter(fctx),
	}
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
			for _, w := range rss {
				w.Item(item)
			}
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

	outputs := make(map[string][]byte, len(kinds))
	for kind, w := range rss {
		outputs[kind] = w.Bytes()
	}
	if outputs[KindACP], err = acp.Bytes(); err != nil {
		return fmt.Errorf("feeds: acp encode: %w", err)
	}

	pipe := s.rdb.Pipeline()
	now := time.Now().UTC()
	for _, kind := range kinds {
		gz, err := gzipBytes(outputs[kind])
		if err != nil {
			return err
		}
		sum := sha256.Sum256(outputs[kind])
		meta := Meta{
			ETag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
			GeneratedAt: now,
			Size:        len(outputs[kind]),
		}
		rawMeta, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		pipe.Set(ctx, dataKey(t.SchemaName, kind), gz, s.staleTTL)
		pipe.Set(ctx, metaKey(t.SchemaName, kind), rawMeta, s.staleTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("feeds: cache write: %w", err)
	}

	s.log.Info("feeds generated",
		slog.String("tenant", t.SchemaName),
		slog.Int("products", count),
		slog.Int("skipped_no_name_or_image", skipped),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	)
	return nil
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

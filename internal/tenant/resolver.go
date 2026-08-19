package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

// ErrUnknownTenant means the host maps to no active store.
var ErrUnknownTenant = errors.New("tenant: unknown store")

// negativeSentinel marks a confirmed-unknown host in Redis so scanner
// traffic cannot stampede Django.
const negativeSentinel = "!"

// maxMemEntries bounds the in-process cache; beyond it a random expired-or-
// oldest entry is evicted. Real tenant counts are tiny — the bound only
// guards against Host-header cardinality abuse.
const maxMemEntries = 10_000

type memEntry struct {
	tenant   *Tenant
	expires  time.Time
	negative bool
}

// Resolver maps inbound hosts to tenants with two cache tiers. Layering:
// process memory (TTL, plus stale-if-error retention) → Redis (shared
// across pods) → Django tenant/resolve. Negative results get a short TTL
// and new tenants therefore resolve within NegativeTTL of creation.
type Resolver struct {
	django  *django.Client
	rdb     *redis.Client
	ttl     time.Duration
	negTTL  time.Duration
	log     *slog.Logger
	metrics *obs.Metrics
	sf      singleflight.Group

	mu  sync.RWMutex
	mem map[string]memEntry
}

func NewResolver(
	dj *django.Client,
	rdb *redis.Client,
	ttl, negTTL time.Duration,
	log *slog.Logger,
	metrics *obs.Metrics,
) *Resolver {
	return &Resolver{
		django:  dj,
		rdb:     rdb,
		ttl:     ttl,
		negTTL:  negTTL,
		log:     log,
		metrics: metrics,
		mem:     make(map[string]memEntry),
	}
}

// Resolve returns the tenant for an inbound host. Redis failures degrade to
// memory+Django; Django failures serve the last known config as stale.
func (r *Resolver) Resolve(ctx context.Context, host string) (*Tenant, error) {
	domain := NormalizeHost(host)
	if domain == "" {
		return nil, ErrUnknownTenant
	}

	if t, err, ok := r.fromMemory(domain, false); ok {
		r.count("hit")
		return t, err
	}

	v, err, _ := r.sf.Do(domain, func() (any, error) {
		return r.resolveSlow(ctx, domain)
	})
	if err != nil {
		return nil, err
	}
	return v.(*Tenant), nil
}

func (r *Resolver) resolveSlow(ctx context.Context, domain string) (*Tenant, error) {
	// Re-check under singleflight: a concurrent caller may have filled it.
	if t, err, ok := r.fromMemory(domain, false); ok {
		return t, err
	}

	if t, err, ok := r.fromRedis(ctx, domain); ok {
		return t, err
	}

	cfg, err := r.django.ResolveTenant(ctx, domain)
	switch {
	case err == nil:
		t := &Tenant{
			TenantConfig:  *cfg,
			Domain:        domain,
			ResolvedAt:    time.Now(),
			SecretsLoaded: true,
		}
		r.storeMemory(domain, memEntry{tenant: t, expires: time.Now().Add(r.ttl)})
		r.storeRedis(ctx, domain, cfg)
		r.count("miss")
		return t, nil

	case errors.Is(err, django.ErrNotFound):
		r.storeMemory(domain, memEntry{negative: true, expires: time.Now().Add(r.negTTL)})
		r.storeRedisNegative(ctx, domain)
		r.count("negative")
		return nil, ErrUnknownTenant

	default:
		// Django unreachable: serve the last known config past its TTL
		// rather than 503ing every agent — tenant config is near-static.
		if t, memErr, ok := r.fromMemory(domain, true); ok && memErr == nil {
			stale := *t
			stale.Stale = true
			r.count("stale")
			r.log.WarnContext(ctx, "serving stale tenant config",
				slog.String("tenant", stale.SchemaName),
				slog.String("host", domain),
				slog.String("error", err.Error()),
			)
			return &stale, nil
		}
		return nil, err
	}
}

// fromMemory returns (tenant, error, found). With allowExpired it also
// returns entries past their TTL — the stale-if-error path.
func (r *Resolver) fromMemory(domain string, allowExpired bool) (*Tenant, error, bool) {
	r.mu.RLock()
	e, ok := r.mem[domain]
	r.mu.RUnlock()
	if !ok {
		return nil, nil, false
	}
	if !allowExpired && time.Now().After(e.expires) {
		return nil, nil, false
	}
	if e.negative {
		if allowExpired && time.Now().After(e.expires) {
			return nil, nil, false
		}
		return nil, ErrUnknownTenant, true
	}
	return e.tenant, nil, true
}

func (r *Resolver) fromRedis(ctx context.Context, domain string) (*Tenant, error, bool) {
	if r.rdb == nil {
		return nil, nil, false
	}
	raw, err := r.rdb.Get(ctx, redisKey(domain)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			r.log.WarnContext(ctx, "tenant redis read failed",
				slog.String("error", err.Error()))
		}
		return nil, nil, false
	}
	if raw == negativeSentinel {
		r.storeMemory(domain, memEntry{negative: true, expires: time.Now().Add(r.negTTL)})
		r.count("negative")
		return nil, ErrUnknownTenant, true
	}
	var cfg django.TenantConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		r.log.WarnContext(ctx, "tenant redis entry corrupt",
			slog.String("error", err.Error()))
		return nil, nil, false
	}
	// SecretsLoaded stays false: storeRedis strips the credential fields.
	t := &Tenant{TenantConfig: cfg, Domain: domain, ResolvedAt: time.Now()}
	r.storeMemory(domain, memEntry{tenant: t, expires: time.Now().Add(r.ttl)})
	r.count("redis_hit")
	return t, nil, true
}

func (r *Resolver) storeRedis(ctx context.Context, domain string, cfg *django.TenantConfig) {
	if r.rdb == nil {
		return
	}
	public := publicConfig(cfg)
	raw, err := json.Marshal(&public)
	if err != nil {
		return
	}
	if err := r.rdb.Set(ctx, redisKey(domain), raw, r.ttl).Err(); err != nil {
		r.log.WarnContext(ctx, "tenant redis write failed",
			slog.String("error", err.Error()))
	}
}

// publicConfig copies a tenant config with the per-tenant CREDENTIALS
// cleared, for the shared Redis cache tier.
//
// Redis DB 4 lives on the same server, behind the same password, as the
// caches Django, Nuxt, media-stream and Celery all use — so marshalling
// the whole config put every tenant's model-provider key and
// agentic-platform bearer in plaintext under a fully predictable key,
// readable from any of those workloads. Credentials stay in the
// process-memory tier only; routes that need one go through
// Resolver.EnsureSecrets.
func publicConfig(cfg *django.TenantConfig) django.TenantConfig {
	public := *cfg
	public.ChatAPIKey = ""
	public.ACPBearerToken = ""
	return public
}

func (r *Resolver) storeRedisNegative(ctx context.Context, domain string) {
	if r.rdb == nil {
		return
	}
	if err := r.rdb.Set(ctx, redisKey(domain), negativeSentinel, r.negTTL).Err(); err != nil {
		r.log.WarnContext(ctx, "tenant redis write failed",
			slog.String("error", err.Error()))
	}
}

func (r *Resolver) storeMemory(domain string, e memEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.mem) >= maxMemEntries {
		r.evictLocked()
	}
	r.mem[domain] = e
}

// evictLocked drops expired entries first, else an arbitrary one. Map
// iteration order is random, so the arbitrary pick is effectively random.
func (r *Resolver) evictLocked() {
	now := time.Now()
	for k, e := range r.mem {
		if now.After(e.expires) {
			delete(r.mem, k)
			return
		}
	}
	for k := range r.mem {
		delete(r.mem, k)
		return
	}
}

func (r *Resolver) count(result string) {
	if r.metrics != nil {
		r.metrics.TenantCache.WithLabelValues(result).Inc()
	}
}

func redisKey(domain string) string {
	return "ag:tenant:" + domain
}

// NormalizeHost lowercases and strips any port from an inbound Host header.
func NormalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// EnsureSecrets returns a tenant whose credential fields are populated.
//
// The Redis tier deliberately stores the public config only (see
// storeRedis), so a tenant resolved from there carries empty
// ChatAPIKey / ACPBearerToken. Routes that authenticate against one —
// /chat and /acp — must call this, or a Redis-tier hit would look
// exactly like a tenant that never configured the feature: a localized
// 404 from chat, a 401 from ACP.
//
// A refresh costs one Django call and repopulates the memory tier, so
// the cost lands at most once per pod per TTL, on two low-volume routes.
func (r *Resolver) EnsureSecrets(
	ctx context.Context, t *Tenant,
) (*Tenant, error) {
	if t == nil || t.SecretsLoaded {
		return t, nil
	}
	cfg, err := r.django.ResolveTenant(ctx, t.Domain)
	if err != nil {
		return nil, err
	}
	full := &Tenant{
		TenantConfig:  *cfg,
		Domain:        t.Domain,
		ResolvedAt:    time.Now(),
		SecretsLoaded: true,
	}
	r.storeMemory(t.Domain, memEntry{
		tenant:  full,
		expires: time.Now().Add(r.ttl),
	})
	return full, nil
}

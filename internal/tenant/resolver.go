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
	// held is the last tenant this pod got from Django for the host, the
	// credentials included. It outlives the tenant a Redis hit puts in
	// its place, so EnsureSecrets can serve it stale while Django cannot
	// be asked. storeLocked keeps it; a negative entry drops it.
	held *Tenant
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

	if t, ok, err := r.fromMemory(domain, false); ok {
		r.count("hit")
		return t, err
	}

	v, err := r.shared(ctx, domain, func(ctx context.Context) (any, error) {
		return r.resolveSlow(ctx, domain)
	})
	if err != nil {
		return nil, err
	}
	return v.(*Tenant), nil
}

// shared runs fn once per key for every concurrent caller. The work is
// detached from the leader's cancellation — the first agent hanging up
// must not fail the resolve for every healthy request waiting on it —
// and stays bounded by the Django client's per-attempt timeout. Each
// caller still stops waiting when its own context ends. A panic in fn
// is logged and returned as an error (obs.Recover).
func (r *Resolver) shared(
	ctx context.Context, key string, fn func(context.Context) (any, error),
) (any, error) {
	detached := context.WithoutCancel(ctx)
	select {
	case res := <-r.sf.DoChan(key, func() (_ any, err error) {
		defer obs.Recover(detached, r.log, "tenant: resolve", &err)
		return fn(detached)
	}):
		return res.Val, res.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Resolver) resolveSlow(ctx context.Context, domain string) (*Tenant, error) {
	// Re-check under singleflight: a concurrent caller may have filled it.
	if t, ok, err := r.fromMemory(domain, false); ok {
		return t, err
	}

	if t, ok, err := r.fromRedis(ctx, domain); ok {
		return t, err
	}

	return r.fromDjango(ctx, domain, false)
}

// fromDjango asks Django for the host and caches its answer: the tenant,
// credentials included, in memory and its public config in Redis; or, for a
// store Django does not know, the negative entry in both, returned as
// ErrUnknownTenant. Every other failure is answered by serveStale.
func (r *Resolver) fromDjango(
	ctx context.Context, domain string, needSecrets bool,
) (*Tenant, error) {
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
	}
	return r.serveStale(ctx, domain, err, needSecrets)
}

// serveStale answers a Django failure (cause) with what this pod last held
// for the host, marked Stale, rather than 503ing every agent: tenant config
// is near-static. needSecrets serves the last tenant with its credentials
// (memEntry.held), otherwise the last tenant of any kind. The credentials
// are served only until secretsUntil: past it a tenant goes without them,
// and a caller that needs them gets cause. Without an answer it returns
// cause.
//
// The answer is cached for the negative TTL: otherwise every request
// re-probes a Django that may be hanging rather than refusing, paying a
// full upstream timeout before getting this answer.
func (r *Resolver) serveStale(
	ctx context.Context, domain string, cause error, needSecrets bool,
) (*Tenant, error) {
	r.mu.RLock()
	e := r.mem[domain]
	r.mu.RUnlock()
	last := e.tenant
	if needSecrets {
		last = e.held
	}
	if last == nil {
		return nil, cause
	}
	stale := *last
	stale.Stale = true
	expires := time.Now().Add(r.negTTL)
	if stale.SecretsLoaded {
		until := r.secretsUntil(&stale)
		switch {
		case time.Now().Before(until):
			if until.Before(expires) {
				expires = until
			}
		case needSecrets:
			return nil, cause
		default:
			stale.TenantConfig = publicConfig(&stale.TenantConfig)
			stale.SecretsLoaded = false
		}
	}
	r.storeMemory(domain, memEntry{tenant: &stale, expires: expires})
	r.count("stale")
	r.log.WarnContext(ctx, "serving stale tenant config",
		slog.String(obs.KeyTenant, stale.SchemaName),
		slog.String("host", domain),
		slog.Bool("credentials", stale.SecretsLoaded),
		slog.String("error", cause.Error()),
	)
	return &stale, nil
}

// secretsUntil is when the credentials of t may no longer be served: one
// TTL after they went stale, two after this pod got them from Django. A
// credential rotated in Django while Django cannot be asked is then
// refused after that, never accepted for the whole outage.
func (r *Resolver) secretsUntil(t *Tenant) time.Time {
	return t.ResolvedAt.Add(2 * r.ttl)
}

// fromMemory returns the cached entry when found: a tenant, or
// ErrUnknownTenant for a negative entry. With allowExpired it also
// returns entries past their TTL — the stale-if-error path.
func (r *Resolver) fromMemory(domain string, allowExpired bool) (*Tenant, bool, error) {
	r.mu.RLock()
	e, ok := r.mem[domain]
	r.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	if !allowExpired && time.Now().After(e.expires) {
		return nil, false, nil
	}
	if e.negative {
		if allowExpired && time.Now().After(e.expires) {
			return nil, false, nil
		}
		return nil, true, ErrUnknownTenant
	}
	return e.tenant, true, nil
}

func (r *Resolver) fromRedis(ctx context.Context, domain string) (*Tenant, bool, error) {
	if r.rdb == nil {
		return nil, false, nil
	}
	raw, err := r.rdb.Get(ctx, redisKey(domain)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			r.log.WarnContext(ctx, "tenant redis read failed",
				slog.String("error", err.Error()))
		}
		return nil, false, nil
	}
	e := memEntry{negative: true, expires: time.Now().Add(r.negTTL)}
	if raw != negativeSentinel {
		var cfg django.TenantConfig
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			r.log.WarnContext(ctx, "tenant redis entry corrupt",
				slog.String("error", err.Error()))
			return nil, false, nil
		}
		// SecretsLoaded stays false: storeRedis strips the credential fields.
		e = memEntry{
			tenant:  &Tenant{TenantConfig: cfg, Domain: domain, ResolvedAt: time.Now()},
			expires: time.Now().Add(r.ttl),
		}
	}
	e = r.fill(domain, e)
	if e.negative {
		r.count("negative")
		return nil, true, ErrUnknownTenant
	}
	r.count("redis_hit")
	return e.tenant, true, nil
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
	r.storeLocked(domain, e)
}

// fill stores e unless the host has a live entry already, and returns the
// entry in effect. A Redis read races the flights that ask Django (a
// resolve's and a top-up's run under different keys): what one of them
// stored meanwhile is Django's newer answer and must stay.
func (r *Resolver) fill(domain string, e memEntry) memEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if live, ok := r.mem[domain]; ok && time.Now().Before(live.expires) {
		return live
	}
	r.storeLocked(domain, e)
	return e
}

// storeLocked stores e, setting what it holds: a tenant fresh from Django
// with its credentials is held itself, a store that is gone holds nothing,
// and anything else keeps what the host's entry held.
func (r *Resolver) storeLocked(domain string, e memEntry) {
	switch {
	case e.negative:
		e.held = nil
	case e.tenant.SecretsLoaded && !e.tenant.Stale:
		e.held = e.tenant
	default:
		e.held = r.mem[domain].held
	}
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

// redisKeyVersion versions the cached TenantConfig shape: a gateway
// that decodes a field an older one never wrote must not read that
// one's entries, or the field reads as its zero value until they
// expire (v2: TenantConfig.Features, which would read as every
// feature off; v3: its cart, catalogue and product alerts; v4:
// AvailableLocales, which would read as the default language alone).
// Bump it with every change to the shape. TestCachedShapeMatchesKeyVersion
// fails on a changed shape until the golden is updated, which prompts the
// bump; a reviewer sees the golden's diff.
const redisKeyVersion = "v5"

func redisKey(domain string) string {
	return "ag:tenant:" + redisKeyVersion + ":" + domain
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
// A refresh is the resolve's own Django call (fromDjango), so it caches
// what the resolve caches, and the cost lands at most once per pod per
// TTL, on two low-volume routes. A store Django no longer knows is
// ErrUnknownTenant, cached as the resolve caches it; while Django cannot
// be asked, the credentials this pod last held are served stale until
// secretsUntil (serveStale).
func (r *Resolver) EnsureSecrets(
	ctx context.Context, t *Tenant,
) (*Tenant, error) {
	if t == nil || t.SecretsLoaded {
		return t, nil
	}
	v, err := r.shared(ctx, t.Domain+"|secrets",
		func(ctx context.Context) (any, error) {
			// Another flight may have filled memory since the caller
			// resolved t.
			if cur, ok, err := r.fromMemory(t.Domain, false); ok &&
				(err != nil || cur.SecretsLoaded) {
				return cur, err
			}
			return r.fromDjango(ctx, t.Domain, true)
		})
	if err != nil {
		return nil, err
	}
	return v.(*Tenant), nil
}

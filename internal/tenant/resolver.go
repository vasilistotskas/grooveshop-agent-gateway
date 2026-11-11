package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// errUnrecognised is a resolve Django answered as it answers any caller:
// the gateway's internal token is not the secret Django holds, most likely
// after a rotation that reached one side only. The body is a valid public
// config, but its empty credentials are not the store's.
var errUnrecognised = errors.New(
	"tenant: Django did not recognise the gateway's internal token")

// errNoCredentials wraps the cause of a credential caller's remembered
// failure. The resolver logged that cause once, when it decided the
// answer (logTopUp), so the middleware does not log it again.
var errNoCredentials = errors.New("tenant: credentials unavailable")

// errAnotherStore is a top-up that answered for a store other than the
// one the caller's request resolved.
var errAnotherStore = errors.New("tenant: the top-up answered for another store")

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
	// be asked. It belongs to a store, not a host: storeLocked keeps it
	// only while the tenant known for the host is that store.
	held *Tenant
	// topUp is the answer a credential caller got when Django could not
	// give the credentials, remembered with held (see answerSecrets).
	topUp topUp
}

// topUp is a credential caller's answer while Django cannot give the
// credentials: the held ones served stale, or the failure that left none.
// It is given again without asking Django until until, the negative TTL
// later, as a stale config is, so a Django that is down or does not
// recognise the gateway is asked once per negative TTL, not per request.
type topUp struct {
	tenant *Tenant
	err    error
	until  time.Time
	schema string // the store it was decided for, for its log line
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

	if t, ok, err := r.fromMemory(domain); ok {
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
	if t, ok, err := r.fromMemory(domain); ok {
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
// ErrUnknownTenant. Every other failure is answered by serveStale, or, for
// a caller that needs the credentials, by answerSecrets.
//
// An answer for a token Django did not recognise is a public config: it is
// cached as one, never as a store without credentials, and logged, and the
// credential callers' answer is decided with it (rememberLocked).
func (r *Resolver) fromDjango(
	ctx context.Context, domain string, needSecrets bool,
) (*Tenant, error) {
	cfg, recognised, err := r.django.ResolveTenant(ctx, domain)
	switch {
	case err == nil:
		now := time.Now()
		t := &Tenant{
			TenantConfig:  *cfg,
			Domain:        domain,
			ResolvedAt:    now,
			SecretsLoaded: recognised,
		}
		var a topUp
		r.mu.Lock()
		r.storeLocked(domain, memEntry{tenant: t, expires: now.Add(r.ttl)})
		if !recognised {
			a = r.rememberLocked(domain, now, errUnrecognised)
		}
		r.mu.Unlock()
		r.storeRedis(ctx, domain, cfg)
		r.count("miss")
		if recognised {
			return t, nil
		}
		r.log.ErrorContext(ctx,
			"Django did not recognise the gateway's internal token",
			slog.String(obs.KeyTenant, cfg.SchemaName),
			slog.String("host", domain),
		)
		r.logTopUp(ctx, domain, errUnrecognised, a)
		if !needSecrets {
			return t, nil
		}
		return a.tenant, a.err

	case errors.Is(err, django.ErrNotFound):
		r.storeMemory(domain, memEntry{negative: true, expires: time.Now().Add(r.negTTL)})
		r.storeRedisNegative(ctx, domain)
		r.count("negative")
		return nil, ErrUnknownTenant
	}
	if needSecrets {
		return r.answerSecrets(ctx, domain, err)
	}
	return r.serveStale(ctx, domain, err)
}

// serveStale answers a Django failure (cause) with the last config this pod
// knows for the host, marked Stale and without credentials (those come
// only from answerSecrets), rather than 503ing every agent: tenant config
// is near-static. It has no age bound. Without a config it returns cause.
//
// The answer is cached for the negative TTL: otherwise every request
// re-probes a Django that may be hanging rather than refusing, paying a
// full upstream timeout before getting this answer. An entry another
// flight made live meanwhile is newer and is answered instead.
func (r *Resolver) serveStale(
	ctx context.Context, domain string, cause error,
) (*Tenant, error) {
	r.mu.Lock()
	e, ok := r.mem[domain]
	switch {
	case ok && time.Now().Before(e.expires):
		r.mu.Unlock()
		if e.negative {
			return nil, ErrUnknownTenant
		}
		return e.tenant, nil
	case !ok || e.negative:
		r.mu.Unlock()
		return nil, cause
	}
	now := time.Now()
	stale := *e.tenant
	stale.Stale = true
	stale.TenantConfig = publicConfig(&stale.TenantConfig)
	stale.SecretsLoaded = false
	r.storeLocked(domain, memEntry{tenant: &stale, expires: now.Add(r.negTTL)})
	a := r.rememberLocked(domain, now, cause)
	r.mu.Unlock()
	r.count("stale")
	r.log.WarnContext(ctx, "serving stale tenant config",
		slog.String(obs.KeyTenant, stale.SchemaName),
		slog.String("host", domain),
		slog.String("error", cause.Error()),
	)
	r.logTopUp(ctx, domain, cause, a)
	return &stale, nil
}

// answerSecrets answers a credential caller Django could not give the
// credentials (cause), through rememberLocked. A tenant with credentials,
// a negative entry or a remembered answer another flight made live
// meanwhile is newer and is answered instead.
func (r *Resolver) answerSecrets(
	ctx context.Context, domain string, cause error,
) (*Tenant, error) {
	now := time.Now()
	r.mu.Lock()
	e, ok := r.mem[domain]
	if t, err, live := e.secretsAnswer(now); ok && live {
		r.mu.Unlock()
		return t, err
	}
	if !ok || e.negative {
		r.mu.Unlock()
		return nil, cause
	}
	a := r.rememberLocked(domain, now, cause)
	r.mu.Unlock()
	r.logTopUp(ctx, domain, cause, a)
	return a.tenant, a.err
}

// rememberLocked decides what a credential caller gets for the host while
// Django cannot give the credentials (cause), and stores it on the entry
// (memEntry.topUp) for the negative TTL, never past secretsUntil: the
// entry's current config with the held credentials, marked Stale, or the
// failure, wrapped in errNoCredentials. It never replaces the entry's
// tenant, which stays the freshest config the pod knows. The entry must
// hold a tenant; the caller holds r.mu and then logs the answer once
// (logTopUp).
func (r *Resolver) rememberLocked(
	domain string, now time.Time, cause error,
) topUp {
	e := r.mem[domain]
	a := topUp{
		err:    fmt.Errorf("%w: %w", errNoCredentials, cause),
		until:  now.Add(r.negTTL),
		schema: e.tenant.SchemaName,
	}
	if e.held != nil {
		if until := r.secretsUntil(e.held); now.Before(until) {
			stale := *e.tenant
			stale.TenantConfig = withCredentials(
				e.tenant.TenantConfig, e.held.TenantConfig)
			stale.ResolvedAt = e.held.ResolvedAt
			stale.SecretsLoaded = true
			stale.Stale = true
			a.tenant, a.err = &stale, nil
			if until.Before(a.until) {
				a.until = until
			}
		}
	}
	e.topUp = a
	r.mem[domain] = e
	return a
}

// logTopUp writes the one line of a credential answer rememberLocked
// decided: a warning for held credentials served stale, an error for the
// failure, unless its cause was logged already (errUnrecognised).
func (r *Resolver) logTopUp(
	ctx context.Context, domain string, cause error, a topUp,
) {
	attrs := []any{
		slog.String(obs.KeyTenant, a.schema),
		slog.String("host", domain),
		slog.String("error", cause.Error()),
	}
	switch {
	case a.tenant != nil:
		r.count("stale")
		r.log.WarnContext(ctx, "serving stale tenant credentials", attrs...)
	case !errors.Is(cause, errUnrecognised):
		r.log.ErrorContext(ctx, "tenant credentials unavailable", attrs...)
	}
}

// secretsAnswer is what a credential caller gets from the entry without
// asking Django, and whether there is such an answer: the store is gone,
// a live tenant with its credentials, or the remembered top-up.
func (e memEntry) secretsAnswer(now time.Time) (*Tenant, error, bool) {
	live := now.Before(e.expires)
	switch {
	case live && e.negative:
		return nil, ErrUnknownTenant, true
	case live && e.tenant.SecretsLoaded:
		return e.tenant, nil, true
	case now.Before(e.topUp.until):
		return e.topUp.tenant, e.topUp.err, true
	}
	return nil, nil, false
}

// secretsUntil is when the credentials of t may no longer be served: one
// TTL after they went stale, two after this pod got them from Django. A
// credential rotated in Django while Django cannot be asked is then
// refused after that, never accepted for the whole outage.
func (r *Resolver) secretsUntil(t *Tenant) time.Time {
	return t.ResolvedAt.Add(2 * r.ttl)
}

// fromMemory returns the live cached entry when found: a tenant, or
// ErrUnknownTenant for a negative entry.
func (r *Resolver) fromMemory(domain string) (*Tenant, bool, error) {
	r.mu.RLock()
	e, ok := r.mem[domain]
	r.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return nil, false, nil
	}
	if e.negative {
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
	e, stored := r.fill(domain, e)
	switch {
	case !stored:
		r.count("hit")
	case e.negative:
		r.count("negative")
	default:
		r.count("redis_hit")
	}
	if e.negative {
		return nil, true, ErrUnknownTenant
	}
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

// withCredentials is cfg with the credentials publicConfig clears taken
// from held: a config the pod knows now, with credentials it got before.
// TestWithCredentialsRestoresWhatPublicConfigClears keeps the two in step.
func withCredentials(cfg, held django.TenantConfig) django.TenantConfig {
	cfg.ChatAPIKey = held.ChatAPIKey
	cfg.ACPBearerToken = held.ACPBearerToken
	return cfg
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
// entry in effect and whether it is e. A Redis read races the flights that
// ask Django (a resolve's and a top-up's run under different keys): what
// one of them stored meanwhile is Django's newer answer and must stay.
func (r *Resolver) fill(domain string, e memEntry) (memEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if live, ok := r.mem[domain]; ok && time.Now().Before(live.expires) {
		return live, false
	}
	r.storeLocked(domain, e)
	return e, true
}

// storeLocked stores e, setting what it holds: a tenant fresh from Django
// with its credentials is held itself; any other tenant of the store the
// host's entry knew keeps what that entry held and its remembered top-up;
// a store that is gone, or another store, holds nothing.
func (r *Resolver) storeLocked(domain string, e memEntry) {
	prev := r.mem[domain]
	e.held, e.topUp = nil, topUp{}
	switch {
	case e.negative:
	case e.tenant.SecretsLoaded && !e.tenant.Stale:
		e.held = e.tenant
	case !prev.negative && prev.tenant != nil &&
		prev.tenant.SchemaName == e.tenant.SchemaName:
		e.held, e.topUp = prev.held, prev.topUp
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
// give the credentials, the ones this pod last held for the store are
// served stale until secretsUntil, and that answer, or the failure, is
// remembered for the negative TTL (answerSecrets).
func (r *Resolver) EnsureSecrets(
	ctx context.Context, t *Tenant,
) (*Tenant, error) {
	if t == nil || t.SecretsLoaded {
		return t, nil
	}
	v, err := r.shared(ctx, t.Domain+"|secrets",
		func(ctx context.Context) (any, error) {
			// Memory may answer: another flight may have filled it since
			// the caller resolved t, or a top-up is remembered.
			r.mu.RLock()
			e, ok := r.mem[t.Domain]
			r.mu.RUnlock()
			if cur, err, live := e.secretsAnswer(time.Now()); ok && live {
				return cur, err
			}
			return r.fromDjango(ctx, t.Domain, true)
		})
	if err != nil {
		return nil, err
	}
	// A caller can join a flight whose answer was fixed before the host
	// moved to another store. Credentials are served only for the store
	// the caller's request resolved.
	full := v.(*Tenant)
	if full.SchemaName != t.SchemaName {
		return nil, errAnotherStore
	}
	return full, nil
}

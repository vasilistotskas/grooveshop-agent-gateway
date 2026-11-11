package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// cacheTTL bounds positive and negative verdicts alike: long enough to
// absorb an AI app's burst of tool calls, short enough that a token
// revoked, or a person removed from a store's team, stops verifying
// within a minute. Django re-checks every forwarded call regardless.
const cacheTTL = 60 * time.Second

// cacheMax is a hard entry cap; hitting it flushes the map (crude, but
// the cache is a shield, not a source of truth).
const cacheMax = 4096

// ErrInvalidToken reports a bearer Django refused with a 401: unknown,
// expired, revoked, bound to another store, or, for a staff token, held
// by someone no longer on the store's team.
var ErrInvalidToken = errors.New("identity: invalid token")

// Probe asks Django who a bearer belongs to at a store. A
// django.ErrUnauthorized is the token being invalid; any other error is
// returned to the caller and not cached.
type Probe[P any] func(
	ctx context.Context, t *tenant.Tenant, token string,
) (P, error)

type cacheEntry[P any] struct {
	profile P
	invalid bool
	expires time.Time
}

// Verifier checks bearers with its Probe, with a small in-memory cache of
// the verdicts. Both MCP servers use one: the shoppers' /mcp with
// /agent/me, the store team's /mcp/admin with /agent/staff/me.
type Verifier[P any] struct {
	probe  Probe[P]
	log    *slog.Logger
	flight singleflight.Group

	mu    sync.Mutex
	cache map[string]cacheEntry[P]
}

func NewVerifier[P any](probe Probe[P], log *slog.Logger) *Verifier[P] {
	return &Verifier[P]{
		probe: probe, log: log, cache: map[string]cacheEntry[P]{},
	}
}

// Verify returns what the probe said about token at t: the profile, or
// ErrInvalidToken.
func (v *Verifier[P]) Verify(
	ctx context.Context, t *tenant.Tenant, token string,
) (P, error) {
	// Key on (tenant, token), never the token alone, and never the raw
	// token. The probe is tenant-scoped, so a verdict is only ever valid
	// for the tenant it was obtained for. Caching by token alone let a
	// hit answer for a DIFFERENT store: a bearer valid for tenant A,
	// replayed against tenant B within the TTL, was admitted with A's
	// shopper profile attached — and in the other direction a token
	// rejected at A was treated as invalid at its own store for the rest
	// of the TTL. Every tenant's ingress targets this same pod pool, so
	// both directions are reachable from outside.
	sum := sha256.Sum256(
		append(append([]byte(t.SchemaName), 0), token...),
	)
	key := hex.EncodeToString(sum[:])

	var zero P
	entry, err := v.entry(ctx, key, t, token)
	switch {
	case err != nil:
		return zero, err
	case entry.invalid:
		return zero, ErrInvalidToken
	}
	return entry.profile, nil
}

// entry is the cached verdict for key, probed when it has none or it has
// expired. Concurrent callers for one key share one probe. The probe is
// detached from the caller who started it, so that caller hanging up
// fails none of the others, and stays bounded by the Django client's
// timeout; each caller still stops waiting when its own context ends. A
// panic in the probe is logged and returned as an error (obs.Recover).
func (v *Verifier[P]) entry(
	ctx context.Context, key string, t *tenant.Tenant, token string,
) (cacheEntry[P], error) {
	v.mu.Lock()
	entry, hit := v.cache[key]
	v.mu.Unlock()
	if hit && time.Now().Before(entry.expires) {
		return entry, nil
	}
	detached := context.WithoutCancel(ctx)
	select {
	case res := <-v.flight.DoChan(key, func() (_ any, err error) {
		defer obs.Recover(detached, v.log, "identity: verify", &err)
		return v.fetch(detached, key, t, token)
	}):
		if res.Err != nil {
			return cacheEntry[P]{}, res.Err
		}
		return res.Val.(cacheEntry[P]), nil
	case <-ctx.Done():
		return cacheEntry[P]{}, ctx.Err()
	}
}

// fetch probes token at t and caches the verdict: a profile, or invalid
// for Django's 401. Any other failure is returned and not cached.
func (v *Verifier[P]) fetch(
	ctx context.Context, key string, t *tenant.Tenant, token string,
) (cacheEntry[P], error) {
	profile, err := v.probe(ctx, t, token)
	entry := cacheEntry[P]{expires: time.Now().Add(cacheTTL)}
	switch {
	case err == nil:
		entry.profile = profile
	case errors.Is(err, django.ErrUnauthorized):
		entry.invalid = true
	default:
		return cacheEntry[P]{}, err
	}
	v.mu.Lock()
	if len(v.cache) >= cacheMax {
		v.cache = map[string]cacheEntry[P]{}
	}
	v.cache[key] = entry
	v.mu.Unlock()
	return entry, nil
}

// Refuse answers a call whose bearer Verify did not accept, with err, the
// same way on both MCP servers:
//   - an invalid token: 401 with the challenge naming metadataURL;
//   - Django's 404: the store switched the surface off and this pod's
//     copy of the store has not caught up yet. 404, as the gateway's own
//     gate answers once it has;
//   - Django's 429: 429 with its Retry-After;
//   - a caller that hung up: 503, and no error line;
//   - anything else: a logged 503.
func Refuse(
	w http.ResponseWriter, r *http.Request, log *slog.Logger,
	metadataURL string, err error,
) {
	switch {
	case errors.Is(err, ErrInvalidToken):
		WriteChallenge(w, metadataURL, `error="invalid_token"`,
			"invalid or expired access token")
		return
	case errors.Is(err, django.ErrNotFound):
		httpmw.WriteJSONError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(err, django.ErrThrottled):
		if apiErr, ok := errors.AsType[*django.APIError](err); ok &&
			apiErr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(
				int(apiErr.RetryAfter.Seconds())))
		}
		httpmw.WriteJSONError(w, http.StatusTooManyRequests,
			"too many requests")
		return
	case !errors.Is(err, context.Canceled):
		log.ErrorContext(r.Context(), "token verification failed",
			slog.String("error", err.Error()))
	}
	httpmw.WriteJSONError(w, http.StatusServiceUnavailable,
		"store temporarily unavailable")
}

// WriteChallenge answers 401 with the RFC 9728 challenge, which sends an
// MCP client to the resource metadata at metadataURL and from there to
// the authorization server. params are the challenge's own attributes,
// such as the error or the scope needed.
func WriteChallenge(
	w http.ResponseWriter, metadataURL, params, message string,
) {
	w.Header().Set("WWW-Authenticate", "Bearer "+params+
		`, resource_metadata="`+metadataURL+`"`)
	httpmw.WriteJSONError(w, http.StatusUnauthorized, message)
}

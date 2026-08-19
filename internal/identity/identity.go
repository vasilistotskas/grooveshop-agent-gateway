// Package identity attaches a linked shopper account to agent requests.
//
// AI agents that completed the OAuth authorization-code flow against the
// store's identity provider (Django allauth.idp) attach their access
// token as `Authorization: Bearer …`. The middleware validates it via
// Django's /agent/me endpoint (which doubles as the profile resource)
// and stashes the result in the request context; account-scoped MCP
// tools forward the same bearer to Django, which enforces scopes.
//
// Anonymous requests pass through untouched — the whole commerce
// surface works without an account; identity only unlocks the my_*
// tools.
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Linked is a validated agent credential. Profile is nil when the token
// is valid but was not granted the `profile` scope — the bearer still
// works for whichever scopes it does carry.
type Linked struct {
	Bearer  string
	Profile *django.AgentProfile
}

type contextKey struct{}

func NewContext(ctx context.Context, l *Linked) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}

func FromContext(ctx context.Context) (*Linked, bool) {
	l, ok := ctx.Value(contextKey{}).(*Linked)
	return l, ok
}

// cacheTTL bounds both positive and negative verification caching: long
// enough to absorb an agent's tool-call burst, short enough that token
// revocation propagates quickly.
const cacheTTL = 60 * time.Second

// cacheMax is a hard entry cap; hitting it flushes the map (crude, but
// the cache is a shield, not a source of truth).
const cacheMax = 4096

type cacheEntry struct {
	profile   *django.AgentProfile
	unauth    bool // token invalid/expired
	scopeless bool // token valid but lacks the profile scope
	expires   time.Time
}

// Verifier validates bearer tokens against Django with a small
// in-memory cache. Tokens are cached by SHA-256 so raw credentials
// never sit in process memory.
type Verifier struct {
	dj  *django.Client
	log *slog.Logger

	mu    sync.Mutex
	cache map[string]cacheEntry
}

func NewVerifier(dj *django.Client, log *slog.Logger) *Verifier {
	return &Verifier{dj: dj, log: log, cache: map[string]cacheEntry{}}
}

// ErrInvalidToken reports a missing/expired/revoked bearer token.
var ErrInvalidToken = errors.New("identity: invalid token")

// Verify resolves the bearer token to a Linked credential.
func (v *Verifier) Verify(
	ctx context.Context, t *tenant.Tenant, token string,
) (*Linked, error) {
	// Key on (tenant, token), never the token alone. The upstream probe
	// below is tenant-scoped, so a verdict is only ever valid for the
	// tenant it was obtained for. Caching by token alone let a hit
	// answer for a DIFFERENT store: a bearer valid for tenant A, replayed
	// against tenant B within the TTL, was admitted with A's shopper
	// profile attached — and in the other direction a token rejected at
	// A was treated as invalid at its own store for the rest of the TTL.
	// Every tenant's ingress targets this same pod pool, so both
	// directions are reachable from outside.
	sum := sha256.Sum256(
		append(append([]byte(t.SchemaName), 0), token...),
	)
	key := hex.EncodeToString(sum[:])

	v.mu.Lock()
	entry, hit := v.cache[key]
	v.mu.Unlock()
	if hit && time.Now().Before(entry.expires) {
		return v.entryToLinked(token, entry)
	}

	profile, err := v.dj.AgentMe(ctx, t.Domain, t.DefaultLocale, token)
	entry = cacheEntry{expires: time.Now().Add(cacheTTL)}
	switch {
	case err == nil:
		entry.profile = profile
	case errors.Is(err, django.ErrUnauthorized):
		entry.unauth = true
	case errors.Is(err, django.ErrForbidden):
		// Valid token without the `profile` scope: the /agent/me probe
		// is denied, but scoped resources the token DOES carry still
		// work — pass the bearer through without a profile.
		entry.scopeless = true
	default:
		return nil, err
	}

	v.mu.Lock()
	if len(v.cache) >= cacheMax {
		v.cache = map[string]cacheEntry{}
	}
	v.cache[key] = entry
	v.mu.Unlock()

	return v.entryToLinked(token, entry)
}

func (v *Verifier) entryToLinked(
	token string, entry cacheEntry,
) (*Linked, error) {
	if entry.unauth {
		return nil, ErrInvalidToken
	}
	return &Linked{Bearer: token, Profile: entry.profile}, nil
}

// Middleware validates an optional Authorization: Bearer header. Runs
// inside the tenant middleware (it needs the tenant for the upstream
// probe). Invalid tokens get the RFC 9728 challenge so MCP clients can
// discover the authorization server and re-run their OAuth flow.
func Middleware(
	v *Verifier, log *slog.Logger,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(auth, "Bearer ")
			if !ok || token == "" {
				next.ServeHTTP(w, r)
				return
			}
			t, tOK := tenant.FromContext(r.Context())
			if !tOK {
				next.ServeHTTP(w, r)
				return
			}

			linked, err := v.Verify(r.Context(), t, token)
			switch {
			case errors.Is(err, ErrInvalidToken):
				w.Header().Set("WWW-Authenticate",
					`Bearer error="invalid_token", resource_metadata=`+
						`"https://`+t.Domain+
						`/.well-known/oauth-protected-resource/mcp"`)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write(
					[]byte(`{"error":"invalid or expired access token"}`))
				return
			case err != nil:
				log.ErrorContext(r.Context(), "token verification failed",
					slog.String("error", err.Error()))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write(
					[]byte(`{"error":"store temporarily unavailable"}`))
				return
			}
			next.ServeHTTP(w, r.WithContext(
				NewContext(r.Context(), linked)))
		})
	}
}

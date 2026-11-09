// Package adminmcp is the store-admin MCP server on /mcp/admin: a store's
// owner and staff ask their own AI app about the store (orders, sales,
// stock, customers, reviews, messages). It shares nothing with the
// shoppers' /mcp beyond the tenant resolver and the Django client: its
// own tools, its own middleware, and its own authorization server.
//
// Staff are platform identities, not the store's customers, so their
// tokens come from Django's staff issuer on the platform's connect host
// (STAFF_CONNECT_HOST), bound to this store's /mcp/admin (RFC 8707).
// Every call needs one. The gateway verifies it against Django's
// /agent/staff/me and forwards it to /api/v1/agent/staff/*, which checks
// the scope, the person's role in the store and their team membership on
// every call. Every tool only reads.
package adminmcp

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
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// Path is the staff MCP server's path on the store's domain.
const Path = "/mcp/admin"

// ReadScopes are the scopes the read tools need, one per area, in the
// order Django's consent page lists them.
var ReadScopes = []string{
	ScopeOrders, ScopeSales, ScopeProfit, ScopeCatalog,
	ScopeCustomers, ScopeReviews, ScopeMessages,
}

const (
	ScopeOrders    = "admin:orders:read"
	ScopeSales     = "admin:sales:read"
	ScopeProfit    = "admin:profit:read"
	ScopeCatalog   = "admin:catalog:read"
	ScopeCustomers = "admin:customers:read"
	ScopeReviews   = "admin:reviews:read"
	ScopeMessages  = "admin:messages:read"
)

// Resource is the RFC 8707 resource a staff token for the store binds to:
// /mcp/admin on its primary domain, as Django requires it.
func Resource(t *tenant.Tenant) string {
	return storefront.Origin(t.PrimaryHost()) + Path
}

// ResourceMetadataURL is the RFC 9728 path-insertion form for Resource.
func ResourceMetadataURL(t *tenant.Tenant) string {
	return storefront.Origin(t.PrimaryHost()) +
		"/.well-known/oauth-protected-resource" + Path
}

// Staff is a verified staff credential: the bearer to forward, and who it
// belongs to.
type Staff struct {
	Bearer  string
	Profile *django.StaffProfile
}

type contextKey struct{}

func withStaff(ctx context.Context, s *Staff) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

func staffFrom(ctx context.Context) (*Staff, bool) {
	s, ok := ctx.Value(contextKey{}).(*Staff)
	return s, ok
}

// cacheTTL bounds positive and negative verdicts alike: long enough for an
// AI app's burst of tool calls, short enough that a token revoked, or a
// person removed from the team, stops verifying within a minute. Django
// re-checks every forwarded call regardless.
const cacheTTL = 60 * time.Second

const cacheMax = 4096

type cacheEntry struct {
	profile *django.StaffProfile
	invalid bool
	expires time.Time
}

// ErrInvalidToken: the bearer is unknown, expired, revoked, bound to
// another store, or its holder is no longer on the store's team.
var ErrInvalidToken = errors.New("adminmcp: invalid token")

// Verifier checks staff bearers against Django, with a small cache keyed
// by SHA-256(schema, token): a verdict obtained for one store never
// answers for another, and no raw token is kept as a key.
type Verifier struct {
	dj *django.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
}

func NewVerifier(dj *django.Client) *Verifier {
	return &Verifier{dj: dj, cache: map[string]cacheEntry{}}
}

func (v *Verifier) Verify(
	ctx context.Context, t *tenant.Tenant, token string,
) (*Staff, error) {
	sum := sha256.Sum256(append(append([]byte(t.SchemaName), 0), token...))
	key := hex.EncodeToString(sum[:])

	v.mu.Lock()
	entry, hit := v.cache[key]
	v.mu.Unlock()
	if !hit || !time.Now().Before(entry.expires) {
		profile, err := v.dj.StaffMe(ctx, t.Domain, t.DefaultLocale, token)
		entry = cacheEntry{expires: time.Now().Add(cacheTTL)}
		switch {
		case err == nil:
			entry.profile = profile
		case errors.Is(err, django.ErrUnauthorized):
			entry.invalid = true
		default:
			return nil, err
		}
		v.mu.Lock()
		if len(v.cache) >= cacheMax {
			v.cache = map[string]cacheEntry{}
		}
		v.cache[key] = entry
		v.mu.Unlock()
	}
	if entry.invalid {
		return nil, ErrInvalidToken
	}
	return &Staff{Bearer: token, Profile: entry.profile}, nil
}

// WriteChallenge answers 401 with the RFC 9728 challenge that sends an MCP
// client to /mcp/admin's metadata, and from there to the staff issuer.
func WriteChallenge(
	w http.ResponseWriter, t *tenant.Tenant, params, message string,
) {
	w.Header().Set("WWW-Authenticate", "Bearer "+params+
		`, resource_metadata="`+ResourceMetadataURL(t)+`"`)
	httpmw.WriteJSONError(w, http.StatusUnauthorized, message)
}

// RequireStaff refuses /mcp/admin to anyone without a valid staff bearer.
// Unlike the shoppers' /mcp there is nothing anonymous here, so a missing
// token gets the challenge naming every read scope, which is what makes a
// client start its OAuth flow.
func RequireStaff(
	v *Verifier, log *slog.Logger,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, ok := tenant.FromContext(r.Context())
			if !ok {
				httpmw.WriteJSONError(w, http.StatusNotFound, "not found")
				return
			}
			token, present := httpmw.BearerToken(r)
			if !present || token == "" {
				WriteChallenge(w, t,
					`scope="`+strings.Join(ReadScopes, " ")+`"`,
					"connect your store's admin account first")
				return
			}
			staff, err := v.Verify(r.Context(), t, token)
			switch {
			case errors.Is(err, ErrInvalidToken):
				WriteChallenge(w, t, `error="invalid_token"`,
					"invalid or expired access token")
				return
			case err != nil:
				log.ErrorContext(r.Context(), "staff token verification failed",
					slog.String("error", err.Error()))
				httpmw.WriteJSONError(w, http.StatusServiceUnavailable,
					"store temporarily unavailable")
				return
			}
			next.ServeHTTP(w, r.WithContext(withStaff(r.Context(), staff)))
		})
	}
}

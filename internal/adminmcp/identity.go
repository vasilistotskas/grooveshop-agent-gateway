// Package adminmcp is the store-admin MCP server on /mcp/admin: a store's
// owner and staff ask their own AI app about the store (orders, sales,
// stock, customers, reviews, messages). It has its own tools, its own
// middleware and its own authorization server; with the shoppers' /mcp
// it shares the tenant resolver, the Django client and what every tool
// answers with (internal/mcptool).
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
	"log/slog"
	"net/http"
	"strings"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/mcptool"
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

// NewVerifier verifies staff bearers against /agent/staff/me. A 404 (the
// store switched access off) or an outage is not cached: the next call
// asks afresh.
func NewVerifier(
	dj *django.Client, log *slog.Logger,
) *identity.Verifier[*django.StaffProfile] {
	return identity.NewVerifier(func(
		ctx context.Context, t *tenant.Tenant, token string,
	) (*django.StaffProfile, error) {
		return dj.StaffMe(ctx, t.Domain, t.DefaultLocale, token)
	}, log)
}

// RequireStaff refuses /mcp/admin to anyone without a valid staff bearer.
// Unlike the shoppers' /mcp there is nothing anonymous here, so a missing
// token gets the challenge naming every read scope, which is what makes a
// client start its OAuth flow. Its challenge sends the client to
// /mcp/admin's metadata, and from there to the staff issuer.
func RequireStaff(
	v *identity.Verifier[*django.StaffProfile], log *slog.Logger,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, ok := tenant.FromContext(r.Context())
			if !ok || !t.Features.StaffAIAccess {
				// A store that has not let its team connect an AI app
				// has no /mcp/admin, as Django has no staff surface.
				httpmw.WriteJSONError(w, http.StatusNotFound, "not found")
				return
			}
			token, present := httpmw.BearerToken(r)
			if !present || token == "" {
				identity.WriteChallenge(w, ResourceMetadataURL(t),
					`scope="`+strings.Join(ReadScopes, " ")+`"`,
					"connect your store's admin account first")
				return
			}
			profile, err := v.Verify(r.Context(), t, token)
			if err != nil {
				identity.Refuse(w, r, log, mcptool.ServerAdmin,
					ResourceMetadataURL(t), err)
				return
			}
			next.ServeHTTP(w, r.WithContext(withStaff(r.Context(),
				&Staff{Bearer: token, Profile: profile})))
		})
	}
}

// Package identity verifies the bearer tokens of both MCP servers and
// attaches a linked shopper account to the shoppers' /mcp requests.
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
//
// The verdict cache (Verifier) and the 401 challenge (WriteChallenge) are
// shared with the store team's /mcp/admin (internal/adminmcp).
package identity

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/httpmw"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/storefront"
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

// NewShopperVerifier verifies shoppers' bearers against /agent/me.
func NewShopperVerifier(
	dj *django.Client,
) *Verifier[*django.AgentProfile] {
	return NewVerifier(func(
		ctx context.Context, t *tenant.Tenant, token string,
	) (*django.AgentProfile, error) {
		profile, err := dj.AgentMe(ctx, t.Domain, t.DefaultLocale, token)
		if errors.Is(err, django.ErrForbidden) {
			// Valid token without the `profile` scope: the /agent/me
			// probe is denied, but scoped resources the token DOES carry
			// still work — pass the bearer through without a profile.
			return nil, nil
		}
		return profile, err
	})
}

// Middleware validates an optional Authorization: Bearer header. Runs
// inside the tenant middleware (it needs the tenant for the upstream
// probe). Invalid tokens get the RFC 9728 challenge so MCP clients can
// discover the authorization server and re-run their OAuth flow.
func Middleware(
	v *Verifier[*django.AgentProfile], log *slog.Logger,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := httpmw.BearerToken(r)
			if !ok || token == "" {
				next.ServeHTTP(w, r)
				return
			}
			t, tOK := tenant.FromContext(r.Context())
			if !tOK {
				next.ServeHTTP(w, r)
				return
			}

			profile, err := v.Verify(r.Context(), t, token)
			switch {
			case errors.Is(err, ErrInvalidToken):
				WriteChallenge(w, storefront.OAuthResourceMetadata(t.Domain),
					`error="invalid_token"`, "invalid or expired access token")
				return
			case err != nil:
				log.ErrorContext(r.Context(), "token verification failed",
					slog.String("error", err.Error()))
				httpmw.WriteJSONError(w, http.StatusServiceUnavailable,
					"store temporarily unavailable")
				return
			}
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(),
				&Linked{Bearer: token, Profile: profile})))
		})
	}
}

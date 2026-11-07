package tenant

import (
	"context"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

// Tenant is the resolved store configuration a request operates under.
type Tenant struct {
	django.TenantConfig

	// Domain is the normalized inbound host the tenant was resolved from.
	// It is what downstream Django calls send as X-Forwarded-Host.
	Domain     string
	ResolvedAt time.Time
	// Stale marks a config served past its TTL because Django was
	// unreachable on refresh (availability over freshness for branding).
	Stale bool
	// SecretsLoaded reports whether ChatAPIKey / ACPBearerToken are
	// populated. They are deliberately NOT written to the shared Redis
	// tier, so a config served from there arrives without them; routes
	// that need one must go through EnsureSecrets. See Resolver.storeRedis.
	SecretsLoaded bool
}

type ctxKey struct{}

func NewContext(ctx context.Context, t *Tenant) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

// FromContext returns the request's tenant. Handlers behind Middleware may
// rely on ok being true.
func FromContext(ctx context.Context) (*Tenant, bool) {
	t, ok := ctx.Value(ctxKey{}).(*Tenant)
	return t, ok
}

// PrimaryHost is the store's primary domain as a lower-case ASCII FQDN. Discovery
// names it rather than the host a request came in on: the store's OAuth metadata
// names it as the resource, and a client refuses to link an account on any other.
// It is also the authority of the store's discovery identifier, which admits no
// IDN form.
func (t *Tenant) PrimaryHost() string {
	domain := t.PrimaryDomain
	if domain == "" {
		domain = t.Domain
	}
	ascii, err := idna.Lookup.ToASCII(strings.TrimSuffix(domain, "."))
	if err != nil {
		ascii = domain
	}
	return strings.ToLower(ascii)
}

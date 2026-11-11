package obs

import (
	"context"
	"log/slog"
	"os"
	"sync"
)

// Log keys the context handler adds. Every line naming a store uses
// KeyTenant (the tenant schema), never another key for the same value.
const (
	KeyRequestID = "request_id"
	KeyTenant    = "tenant"
)

// NewLogger returns a JSON slog logger writing to stdout. Vector tails pod
// stdout in the cluster, so structured JSON is the only output format.
func NewLogger(level slog.Level, env, version string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(NewContextHandler(h)).With(
		slog.String("service", "agent-gateway"),
		slog.String("env", env),
		slog.String("version", version),
	)
}

type ctxKeyRequestID struct{}

// WithRequestID binds the request id that every line logged with the
// returned context carries.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID{}, id)
}

// RequestID returns the request id bound by WithRequestID.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return id
}

// tenantSlot is filled after the context carrying it was handed out: the
// tenant is resolved inside routing, below the middleware that logs the
// request's access line, so a plain context value set there would be
// invisible to it.
type tenantSlot struct {
	mu     sync.Mutex
	schema string
}

type ctxKeyTenant struct{}

// WithTenantSlot gives the request a tenant that SetTenant fills later.
func WithTenantSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyTenant{}, &tenantSlot{})
}

// SetTenant records the request's tenant schema for every line logged
// with a context derived from WithTenantSlot's, before or after this call.
func SetTenant(ctx context.Context, schema string) {
	if s, ok := ctx.Value(ctxKeyTenant{}).(*tenantSlot); ok {
		s.mu.Lock()
		s.schema = schema
		s.mu.Unlock()
	}
}

func tenantOf(ctx context.Context) string {
	s, ok := ctx.Value(ctxKeyTenant{}).(*tenantSlot)
	if !ok {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.schema
}

// contextHandler adds the request id and tenant the record's context
// carries. A record that already names one keeps its own: the stale
// tenant line names the tenant it resolved before the request's is set.
// That check reads the record's own attributes only, not those a logger
// carries from With, so a logger bound to a tenant is for detached work.
type contextHandler struct {
	slog.Handler
}

// NewContextHandler wraps h so records logged with a request's context
// carry its request id and tenant.
func NewContextHandler(h slog.Handler) slog.Handler {
	return contextHandler{h}
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	var hasID, hasTenant bool
	r.Attrs(func(a slog.Attr) bool {
		hasID = hasID || a.Key == KeyRequestID
		hasTenant = hasTenant || a.Key == KeyTenant
		return true
	})
	if id := RequestID(ctx); id != "" && !hasID {
		r.AddAttrs(slog.String(KeyRequestID, id))
	}
	if t := tenantOf(ctx); t != "" && !hasTenant {
		r.AddAttrs(slog.String(KeyTenant, t))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

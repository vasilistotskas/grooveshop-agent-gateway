package adminmcp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/identity"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

// errorLines records the server named by each record logged at error
// level.
type errorLines struct {
	mu      sync.Mutex
	servers []string
}

func (e *errorLines) Enabled(context.Context, slog.Level) bool { return true }

func (e *errorLines) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelError {
		return nil
	}
	server := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "server" {
			server = a.Value.String()
		}
		return true
	})
	e.mu.Lock()
	e.servers = append(e.servers, server)
	e.mu.Unlock()
	return nil
}

func (e *errorLines) WithAttrs([]slog.Attr) slog.Handler { return e }
func (e *errorLines) WithGroup(string) slog.Handler      { return e }

func (e *errorLines) logged() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.servers
}

// requireStaffOn sends a call with token on ctx through RequireStaff in
// front of a handler that must not run, and returns the response.
func requireStaffOn(
	t *testing.T, ctx context.Context,
	v *identity.Verifier[*django.StaffProfile], log *slog.Logger,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	h := RequireStaff(v, log)(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {
			t.Error("an unverified call must not reach the handler")
		}))
	req := httptest.NewRequestWithContext(
		tenant.NewContext(ctx, acme()), http.MethodPost, Path,
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A tool handler that panics is answered with a tool error over the
// stateless handler, and the process lives on: the SDK runs a handler on
// a goroutine of its own, out of reach of the HTTP recover. Here the
// store's client is missing, so the first call to it dereferences nil.
func TestAPanickingToolIsAnsweredOverTheStatelessHandler(t *testing.T) {
	lines := &toolServers{}
	staff := &Staff{Bearer: goodToken, Profile: &django.StaffProfile{
		Scopes: []django.StaffScope{
			{Scope: ScopeOrders, Granted: true, Allowed: true},
		},
	}}
	h := Handler(Deps{Log: slog.New(lines), Version: "test"})
	session := connect(t, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			ctx := withStaff(tenant.NewContext(r.Context(), acme()), staff)
			h.ServeHTTP(w, r.WithContext(ctx))
		}))

	res, err := session.CallTool(t.Context(),
		&mcp.CallToolParams{Name: "list_orders"})

	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text,
		`"code":"unavailable"`)
	lines.mu.Lock()
	defer lines.mu.Unlock()
	assert.Equal(t, []string{"mcp_admin"}, lines.servers)
}

// A caller that hung up while its staff bearer was verified is not an
// error. In a synctest bubble, a middleware that kept waiting would
// deadlock the bubble and fail the test at once.
func TestACallerThatHungUpIsNotAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		v := identity.NewVerifier(func(
			context.Context, *tenant.Tenant, string,
		) (*django.StaffProfile, error) {
			<-release
			return nil, nil
		}, quietLog())
		lines := &errorLines{}
		ctx, hangUp := context.WithCancel(t.Context())
		hangUp()

		rec := requireStaffOn(t, ctx, v, slog.New(lines), goodToken)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Empty(t, lines.logged())
		close(release)
		synctest.Wait()
	})
}

// A Bearer header with no token is no credential: the challenge naming
// every read scope, and Django is not asked.
func TestAnEmptyBearerIsChallengedWithEveryReadScope(t *testing.T) {
	f := &fakeDjango{}
	dj := f.serve(t)

	rec := requireStaffOn(t, t.Context(), NewVerifier(dj, quietLog()),
		quietLog(), "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `Bearer scope="`+strings.Join(ReadScopes, " ")+`", `+
		`resource_metadata="https://shop.acme.test`+
		`/.well-known/oauth-protected-resource/mcp/admin"`,
		rec.Header().Get("WWW-Authenticate"))
	assert.Zero(t, f.meCalls.Load())
}

// /agent/staff/me answering anything but a profile, a 401, a 404 or a
// 429 is an outage: 503, logged against /mcp/admin, the handler never
// reached, and nothing cached, so the next call asks again. A 403 is no
// valid verdict here: Django has none to give for a staff token.
func TestAFailedStaffVerificationIsUnavailableAndNotCached(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		closed bool
	}{
		{name: "403", status: http.StatusForbidden},
		{name: "500", status: http.StatusInternalServerError},
		{name: "502", status: http.StatusBadGateway},
		{name: "302", status: http.StatusFound},
		{name: "a closed listener", closed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					probes.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if tc.status == http.StatusFound {
						w.Header().Set("Location", "/elsewhere")
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"detail":"no"}`))
				}))
			t.Cleanup(srv.Close)
			if tc.closed {
				srv.Close()
			}
			// A short timeout: a closed port can take its time to refuse.
			v := NewVerifier(django.New(srv.URL+"/api/v1", "api.acme.test",
				"secret", time.Second, quietLog(), obs.NewMetrics()),
				quietLog())
			lines := &errorLines{}

			var seen []int32
			for range 2 {
				rec := requireStaffOn(t, t.Context(), v, slog.New(lines),
					goodToken)
				assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
				seen = append(seen, probes.Load())
			}

			assert.Equal(t, []string{"mcp_admin", "mcp_admin"},
				lines.logged())
			if !tc.closed {
				assert.Greater(t, seen[1], seen[0], "asked again")
			}
		})
	}
}

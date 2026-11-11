package identity

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
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

// meAnswers is /agent/me answering every call with status, header and
// body, as Django does.
func meAnswers(
	t *testing.T, status int, header http.Header, body string,
) *Verifier[*django.AgentProfile] {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range header {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(srv.Close)
	return NewShopperVerifier(django.New(srv.URL+"/api/v1",
		"api.example.test", "s", 2*time.Second, testLogger(), nil),
		testLogger())
}

// throughMiddleware sends a bearer call on ctx through Middleware and
// reports the response and the servers its error lines named.
func throughMiddleware(
	t *testing.T, ctx context.Context, v *Verifier[*django.AgentProfile],
) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	lines := &errorLines{}
	h := Middleware(v, slog.New(lines))(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {
			t.Error("an unverified call must not reach the handler")
		}))
	req := httptest.NewRequestWithContext(
		tenant.NewContext(ctx, testTenant()), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, lines.logged()
}

// Django answers /agent/me 404 once the store switched agent commerce
// off. The store has no /mcp then, which is what the tenant gate answers
// once this pod's copy of the store catches up: the same 404, not an
// outage.
func TestMiddlewareStoreSwitchedOffIsNotFound(t *testing.T) {
	v := meAnswers(t, http.StatusNotFound, nil, `{"detail":"Not found."}`)

	rec, errorsLogged := throughMiddleware(t, t.Context(), v)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":"not found"}`, rec.Body.String())
	assert.Empty(t, errorsLogged)
}

// A throttled verification is a 429 the client can back off on, not an
// outage.
func TestMiddlewareThrottledVerificationIsPassedOn(t *testing.T) {
	v := meAnswers(t, http.StatusTooManyRequests,
		http.Header{"Retry-After": {"17"}},
		`{"detail":"Request was throttled."}`)

	rec, errorsLogged := throughMiddleware(t, t.Context(), v)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "17", rec.Header().Get("Retry-After"))
	assert.Empty(t, errorsLogged)
}

// An outage is still a 503, logged with the server it failed on.
func TestMiddlewareOutageIsAnError(t *testing.T) {
	v := meAnswers(t, http.StatusBadGateway, nil, "")

	rec, errorsLogged := throughMiddleware(t, t.Context(), v)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, []string{"mcp"}, errorsLogged)
}

// A caller that hung up while its bearer was verified is not an error.
// In a synctest bubble, a middleware that kept waiting would deadlock the
// bubble and fail the test at once.
func TestMiddlewareCallerThatHungUpIsNotAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		v := NewVerifier(func(
			context.Context, *tenant.Tenant, string,
		) (*django.AgentProfile, error) {
			<-release
			return nil, nil
		}, testLogger())
		ctx, hangUp := context.WithCancel(t.Context())
		hangUp()

		rec, errorsLogged := throughMiddleware(t, ctx, v)

		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Empty(t, errorsLogged)
		close(release)
		synctest.Wait()
	})
}

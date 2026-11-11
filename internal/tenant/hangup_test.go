package tenant

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// errorLines counts the records logged at error level.
type errorLines struct{ n atomic.Int32 }

func (e *errorLines) Enabled(context.Context, slog.Level) bool { return true }

func (e *errorLines) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		e.n.Add(1)
	}
	return nil
}

func (e *errorLines) WithAttrs([]slog.Attr) slog.Handler { return e }
func (e *errorLines) WithGroup(string) slog.Handler      { return e }

// hungUp runs one request for secretsHost, whose caller has already hung
// up, through the middleware mw builds, and reports the status and the
// error lines it logged.
func hungUp(
	t *testing.T, mw func(*slog.Logger) func(http.Handler) http.Handler,
) (int, int32) {
	t.Helper()
	lines := &errorLines{}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler must not run without a tenant")
	})
	ctx, hangUp := context.WithCancel(t.Context())
	hangUp()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"http://ignored/", nil)
	req.Host = secretsHost
	rec := httptest.NewRecorder()
	// Django holds the call, so a middleware that kept waiting for it
	// would never return: fail instead of hanging until the test timeout.
	done := make(chan struct{})
	go func() {
		defer close(done)
		mw(slog.New(lines))(next).ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(hungUpBound):
		t.Fatal("the middleware kept waiting for a caller that hung up")
	}
	return rec.Code, lines.n.Load()
}

// hungUpBound is far longer than a middleware that stops waiting takes;
// it only keeps a broken one from hanging the suite.
const hungUpBound = 30 * time.Second

// A caller that hung up while its store was resolved is not an error.
func TestMiddlewareCallerThatHungUpIsNotAnError(t *testing.T) {
	f := newFakeDjango(t)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	f.mu.Lock()
	f.hold = hold
	f.mu.Unlock()
	r := newTestResolver(t, f)

	status, errorsLogged := hungUp(t,
		func(log *slog.Logger) func(http.Handler) http.Handler {
			return Middleware(r, log)
		})

	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Zero(t, errorsLogged)
}

// Nor is one that hung up while its store's credentials were topped up.
func TestMiddlewareWithSecretsCallerThatHungUpIsNotAnError(t *testing.T) {
	f := newFakeDjango(t)
	_, podB, _ := twoPods(t, f, newFakeRedis(t))
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	f.mu.Lock()
	f.hold = hold
	f.mu.Unlock()

	status, errorsLogged := hungUp(t,
		func(log *slog.Logger) func(http.Handler) http.Handler {
			return MiddlewareWithSecrets(podB, log)
		})

	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Zero(t, errorsLogged)
}

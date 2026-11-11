package django

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/obs"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(
		"..", "..", "testdata", "fixtures", "django", name,
	))
	require.NoError(t, err)
	return b
}

func newTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	return New(srvURL+"/api/v1", "api.example.test", "test-secret",
		2*time.Second, testLogger(), nil)
}

func TestResolveTenantDecodesFixture(t *testing.T) {
	var gotProto, gotHost, gotDomain string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/tenant/resolve", r.URL.Path)
			gotProto = r.Header.Get("X-Forwarded-Proto")
			gotHost = r.Header.Get("X-Forwarded-Host")
			gotDomain = r.URL.Query().Get("domain")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "tenant_resolve_demostore.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cfg, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.NoError(t, err)

	assert.Equal(t, "https", gotProto)
	assert.Equal(t, "api.example.test", gotHost)
	assert.Equal(t, "shop.example.test", gotDomain)

	assert.Equal(t, "demostore", cfg.SchemaName)
	assert.Equal(t, "Demo Store", cfg.StoreName)
	assert.Equal(t, "el", cfg.DefaultLocale)
	assert.Equal(t, "EUR", cfg.DefaultCurrency)
	assert.Equal(t, "shop.example.test", cfg.PrimaryDomain)
	assert.Equal(t, StoreFeatures{
		Catalogue: true, Cart: true, ProductReviews: true, ProductAlerts: true,
		Favourites: true, Loyalty: true,
	}, cfg.Features)
	assert.Equal(t, "sk-test-fake", cfg.ChatAPIKey)
	assert.Equal(t, "acp-bearer-demostore-fixture", cfg.ACPBearerToken)
}

func TestResolveTenantNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail": "Store not found."}`))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ResolveTenant(context.Background(), "nope.example.test")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "Store not found.", apiErr.Detail)
}

func TestGetRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) < 3 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "tenant_resolve_demostore.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cfg, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load())
	assert.Equal(t, "demostore", cfg.SchemaName)
}

func TestGetDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrValidation)
	assert.Equal(t, int32(1), calls.Load())
}

func TestGetPersistentServerErrorWrapsUpstreamDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamDown)
}

func TestConnectionRefusedWrapsUpstreamDown(t *testing.T) {
	c := New("http://127.0.0.1:1/api/v1", "api.example.test", "test-secret",
		200*time.Millisecond, testLogger(), nil)
	_, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamDown)
}

func TestRedirectIsAnError(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var calls, followed atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/order", func(w http.ResponseWriter,
				r *http.Request) {
				calls.Add(1)
				http.Redirect(w, r, "/elsewhere?token=t", http.StatusFound)
			})
			mux.HandleFunc("/elsewhere", func(w http.ResponseWriter,
				_ *http.Request) {
				followed.Add(1)
				_, _ = w.Write([]byte(`{}`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			var logs bytes.Buffer
			c := New(srv.URL+"/api/v1", "api.example.test", "test-secret",
				2*time.Second, slog.New(slog.NewTextHandler(&logs, nil)), nil)

			var out map[string]any
			req := request{path: "/order", host: "shop.example.test", out: &out}
			var err error
			if method == http.MethodGet {
				err = c.get(context.Background(), req)
			} else {
				req.body = map[string]string{"a": "b"}
				err = c.send(context.Background(), method, req)
			}

			require.ErrorIs(t, err, ErrUpstreamDown)
			assert.Equal(t, int32(1), calls.Load(), "a redirect is not retried")
			assert.Zero(t, followed.Load(), "a redirect is not followed")
			assert.Contains(t, logs.String(), "location=/elsewhere")
			assert.NotContains(t, logs.String(), "token")
		})
	}
}

func TestGetDoesNotRetryTimeout(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
	defer srv.Close()
	defer close(release)

	c := New(srv.URL+"/api/v1", "api.example.test", "test-secret",
		100*time.Millisecond, testLogger(), nil)
	_, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.ErrorIs(t, err, ErrUpstreamDown)
	assert.Equal(t, int32(1), calls.Load())
}

func TestGetRetriesDroppedConnection(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) < 3 {
				conn, _, err := http.NewResponseController(w).Hijack()
				require.NoError(t, err)
				_ = conn.Close()
				return
			}
			_, _ = w.Write(fixture(t, "tenant_resolve_demostore.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load())
}

// A call whose caller's context ended is the caller's doing: not an
// upstream failure, not retried, not warned about, counted as canceled.
func TestCallerGoneIsNotAnUpstreamFailure(t *testing.T) {
	calls := []struct {
		name   string
		method string
		call   func(*Client, context.Context) error
	}{
		{"get", http.MethodGet, func(c *Client, ctx context.Context) error {
			return c.get(ctx, request{path: "/cart", host: "shop.example.test"})
		}},
		{"send", http.MethodPost, func(c *Client, ctx context.Context) error {
			return c.send(ctx, http.MethodPost,
				request{path: "/cart", host: "shop.example.test"})
		}},
	}
	ends := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(50*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(),
				50*time.Millisecond)
		}, context.DeadlineExceeded},
	}
	for _, call := range calls {
		for _, end := range ends {
			t.Run(call.name+"/"+end.name, func(t *testing.T) {
				var hits atomic.Int32
				release := make(chan struct{})
				srv := httptest.NewServer(http.HandlerFunc(
					func(_ http.ResponseWriter, r *http.Request) {
						hits.Add(1)
						select {
						case <-r.Context().Done():
						case <-release:
						}
					}))
				defer srv.Close()
				defer close(release)
				var logs bytes.Buffer
				metrics := obs.NewMetrics()
				c := New(srv.URL+"/api/v1", "api.example.test", "s",
					2*time.Second,
					slog.New(slog.NewTextHandler(&logs, nil)), metrics)

				ctx, cancel := end.ctx()
				defer cancel()
				err := call.call(c, ctx)

				require.ErrorIs(t, err, end.want)
				assert.NotErrorIs(t, err, ErrUpstreamDown)
				assert.Equal(t, int32(1), hits.Load())
				assert.Empty(t, logs.String())
				assert.Equal(t, map[string]float64{
					"/cart " + call.method + " canceled": 1,
				}, upstreamCounts(t, metrics))
			})
		}
	}
}

// upstreamCounts is gateway_upstream_requests_total by
// "endpoint method code".
func upstreamCounts(t *testing.T, m *obs.Metrics) map[string]float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "gateway_upstream_requests_total" {
			continue
		}
		for _, metric := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			out[labels["endpoint"]+" "+labels["method"]+" "+
				labels["code"]] = metric.GetCounter().GetValue()
		}
	}
	return out
}

func TestPingIsOneAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			assert.Equal(t, "/api/v1/health/live", r.URL.Path)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	require.ErrorIs(t, c.Ping(context.Background()), ErrUpstreamDown)
	assert.Equal(t, int32(1), calls.Load())
}

func TestAPIErrorSentinels(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{404, ErrNotFound},
		{409, ErrConflict},
		{429, ErrThrottled},
		{400, ErrValidation},
		{500, ErrUpstreamDown},
		{503, ErrUpstreamDown},
	}
	for _, tc := range cases {
		err := &APIError{Status: tc.status, Detail: "x"}
		assert.True(t, errors.Is(err, tc.want), "status %d", tc.status)
	}
}

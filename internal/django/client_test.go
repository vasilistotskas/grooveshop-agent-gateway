package django

import (
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
			require.Equal(t, "/api/v1/tenant/resolve", r.URL.Path)
			gotProto = r.Header.Get("X-Forwarded-Proto")
			gotHost = r.Header.Get("X-Forwarded-Host")
			gotDomain = r.URL.Query().Get("domain")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture(t, "tenant_resolve_webside.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cfg, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.NoError(t, err)

	assert.Equal(t, "https", gotProto)
	assert.Equal(t, "api.example.test", gotHost)
	assert.Equal(t, "shop.example.test", gotDomain)

	assert.Equal(t, "webside", cfg.SchemaName)
	assert.Equal(t, "Webside", cfg.StoreName)
	assert.Equal(t, "el", cfg.DefaultLocale)
	assert.Equal(t, "EUR", cfg.DefaultCurrency)
	assert.Equal(t, "shop.example.test", cfg.PrimaryDomain)
	assert.True(t, cfg.LoyaltyEnabled)
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
			_, _ = w.Write(fixture(t, "tenant_resolve_webside.json"))
		}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cfg, err := c.ResolveTenant(context.Background(), "shop.example.test")
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load())
	assert.Equal(t, "webside", cfg.SchemaName)
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

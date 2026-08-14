//go:build integration

// Package integration holds tests that need real infrastructure (Docker).
// Run with: go test -tags integration ./internal/integration/...
package integration

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
)

func startRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx,
		testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "redis:8-alpine",
				ExposedPorts: []string{"6379/tcp"},
				WaitingFor:   wait.ForListeningPort("6379/tcp"),
			},
			Started: true,
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "6379")
	require.NoError(t, err)

	rdb := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	require.NoError(t, rdb.Ping(ctx).Err())
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError + 1,
	}))
}

func TestResolverSharesRedisTierAcrossInstances(t *testing.T) {
	rdb := startRedis(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("domain") == "shop.example.test" {
				_, _ = w.Write([]byte(`{
					"schemaName": "webside",
					"storeName": "Webside",
					"defaultLocale": "el",
					"defaultCurrency": "EUR",
					"primaryDomain": "shop.example.test",
					"loyaltyEnabled": true,
					"blogEnabled": true
				}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail": "Store not found."}`))
		}))
	defer srv.Close()

	dj := django.New(srv.URL+"/api/v1", "api.example.test", "test-secret",
		time.Second, quietLogger(), nil)

	// Two resolver instances simulate two gateway pods sharing Redis.
	podA := tenant.NewResolver(dj, rdb, time.Minute, time.Minute,
		quietLogger(), nil)
	podB := tenant.NewResolver(dj, rdb, time.Minute, time.Minute,
		quietLogger(), nil)

	ctx := context.Background()
	got, err := podA.Resolve(ctx, "shop.example.test")
	require.NoError(t, err)
	assert.Equal(t, "webside", got.SchemaName)
	assert.Equal(t, int32(1), calls.Load())

	got, err = podB.Resolve(ctx, "shop.example.test")
	require.NoError(t, err)
	assert.Equal(t, "webside", got.SchemaName)
	assert.Equal(t, int32(1), calls.Load(),
		"pod B must be served from the shared Redis tier")

	// Negative results are shared too.
	_, err = podA.Resolve(ctx, "nope.example.test")
	assert.ErrorIs(t, err, tenant.ErrUnknownTenant)
	callsAfterNegative := calls.Load()

	_, err = podB.Resolve(ctx, "nope.example.test")
	assert.ErrorIs(t, err, tenant.ErrUnknownTenant)
	assert.Equal(t, callsAfterNegative, calls.Load(),
		"pod B must see the shared negative sentinel")

	// The Redis entry is namespaced per plan: ag:tenant:{host}.
	val, err := rdb.Get(ctx, "ag:tenant:nope.example.test").Result()
	require.NoError(t, err)
	assert.Equal(t, "!", val)
}

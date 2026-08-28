//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

func TestSigningKeysDistinctPerSchema(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	keys := ucp.NewKeys(rdb)

	alpha, err := keys.ForSchema(ctx, "alpha")
	require.NoError(t, err)
	beta, err := keys.ForSchema(ctx, "beta")
	require.NoError(t, err)
	assert.NotEqual(t, alpha.KID, beta.KID,
		"schemas must not share a signing identity")

	// A fresh instance (another pod) loads the same persisted keys.
	again, err := ucp.NewKeys(rdb).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	assert.Equal(t, alpha.KID, again.KID)
	assert.Equal(t, alpha.Public, again.Public)
}

// No schema adopts a pre-existing platform-wide key. The gateway once
// seeded tenant #1 from a global ag:ucp:signing_key left over from
// before keys went per-schema; that adoption is gone, so a stray global
// key is inert and every schema mints its own identity.
func TestNoSchemaAdoptsAGlobalKey(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	require.NoError(t,
		rdb.Set(ctx, "ag:ucp:signing_key", "stale-global-seed", 0).Err())

	key, err := ucp.NewKeys(rdb).ForSchema(ctx, "alpha")
	require.NoError(t, err)

	seed, err := rdb.Get(ctx, "ag:alpha:ucp:signing_key").Result()
	require.NoError(t, err)
	assert.NotEqual(t, "stale-global-seed", seed,
		"a leftover global key must never become a schema's identity")
	assert.NotEmpty(t, key.KID)
}

func TestProfilePublishesOnlyOwnKey(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	keys := ucp.NewKeys(rdb)

	alpha, err := keys.ForSchema(ctx, "alpha")
	require.NoError(t, err)
	beta, err := keys.ForSchema(ctx, "beta")
	require.NoError(t, err)

	serve := func(schema string) []map[string]string {
		tn := &tenant.Tenant{
			TenantConfig: django.TenantConfig{
				SchemaName:      schema,
				DefaultLocale:   "el",
				DefaultCurrency: "EUR",
			},
			Domain: schema + ".example.test",
		}
		req := httptest.NewRequest(http.MethodGet, "/.well-known/ucp", nil)
		req = req.WithContext(tenant.NewContext(req.Context(), tn))
		rec := httptest.NewRecorder()
		ucp.ProfileHandler(keys, "test").ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		var profile struct {
			Keys []map[string]string `json:"keys"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &profile))
		return profile.Keys
	}

	alphaKeys := serve("alpha")
	require.Len(t, alphaKeys, 1)
	assert.Equal(t, alpha.KID, alphaKeys[0]["kid"])

	betaKeys := serve("beta")
	require.Len(t, betaKeys, 1)
	assert.Equal(t, beta.KID, betaKeys[0]["kid"])
	assert.NotEqual(t, alphaKeys[0]["kid"], betaKeys[0]["kid"])
}

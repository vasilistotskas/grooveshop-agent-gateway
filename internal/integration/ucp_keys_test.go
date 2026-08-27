//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
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

const legacySigningKeyRedisKey = "ag:ucp:signing_key"

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

func TestWebsideAdoptsLegacySigningKey(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()

	// A legacy global key from before keys went per-schema.
	legacyPub, legacyPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	encoded := base64.StdEncoding.EncodeToString(legacyPriv.Seed())
	require.NoError(t,
		rdb.Set(ctx, legacySigningKeyRedisKey, encoded, 0).Err())

	keys := ucp.NewKeys(rdb)
	webside, err := keys.ForSchema(ctx, "webside")
	require.NoError(t, err)
	assert.Equal(t, legacyPub, webside.Public,
		"webside must keep the platform-registered legacy key")

	// The adoption copies: the legacy key survives for manual cleanup.
	kept, err := rdb.Get(ctx, legacySigningKeyRedisKey).Result()
	require.NoError(t, err)
	assert.Equal(t, encoded, kept)

	// Other schemas never inherit the legacy identity.
	other, err := keys.ForSchema(ctx, "alpha")
	require.NoError(t, err)
	assert.NotEqual(t, webside.KID, other.KID)
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

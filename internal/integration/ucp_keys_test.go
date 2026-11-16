//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/config"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/tenant"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// sealedKeyName is the record a store's signing key is held in.
func sealedKeyName(schema string) string {
	return "ag:" + schema + ":ucp:signing_key:es256:aes256gcm"
}

// unsealedKeyName is the record releases before the sealed one held a
// store's key in.
func unsealedKeyName(schema string) string {
	return "ag:" + schema + ":ucp:signing_key:es256"
}

// testKEK is a fresh key-encryption key.
func testKEK(t *testing.T) []byte {
	t.Helper()
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	return kek
}

// newKeys is the store of signing keys sealed under kek.
func newKeys(t *testing.T, rdb *redis.Client, kek []byte) *ucp.Keys {
	t.Helper()
	keys, err := ucp.NewKeys(rdb, kek)
	require.NoError(t, err)
	return keys
}

// unsealedScalar writes schema's key as releases before the sealed one
// stored it, and returns the scalar's base64.
func unsealedScalar(t *testing.T, rdb *redis.Client, schema string) string {
	t.Helper()
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	raw[0] &= 0x7f // below the P-256 order
	encoded := base64.StdEncoding.EncodeToString(raw)
	require.NoError(t, rdb.Set(context.Background(), unsealedKeyName(schema),
		encoded, 0).Err())
	return encoded
}

func TestSigningKeysDistinctPerSchema(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	keys := newKeys(t, rdb, kek)

	alpha, err := keys.ForSchema(ctx, "alpha")
	require.NoError(t, err)
	beta, err := keys.ForSchema(ctx, "beta")
	require.NoError(t, err)
	assert.NotEqual(t, alpha.KID, beta.KID,
		"schemas must not share a signing identity")

	// A fresh instance (another pod) loads the same persisted keys.
	again, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	assert.Equal(t, alpha.KID, again.KID)
	assert.Equal(t, alpha.JWK(), again.JWK())
}

// Redis holds a store's key sealed: not the scalar, not its base64, and
// nothing under the unsealed record's name.
func TestSigningKeyIsSealedInRedis(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	key, err := newKeys(t, rdb, testKEK(t)).ForSchema(ctx, "alpha")
	require.NoError(t, err)

	scalar, err := key.Private.Bytes()
	require.NoError(t, err)
	stored, err := rdb.Get(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.NotContains(t, stored, base64.StdEncoding.EncodeToString(scalar))
	raw, err := base64.StdEncoding.DecodeString(stored)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), string(scalar))

	n, err := rdb.Exists(ctx, unsealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.Zero(t, n, "no key is written unsealed")
}

// A sealed key opens only where it was sealed. Another key-encryption
// key, or the record copied to another store, is an error: never a key
// minted over it, which would rotate the store's published key.
func TestSealedSigningKeyIsNeverReplaced(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	alpha, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	sealed, err := rdb.Get(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)

	_, err = newKeys(t, rdb, testKEK(t)).ForSchema(ctx, "alpha")
	require.Error(t, err, "another key-encryption key must not open it")

	require.NoError(t, rdb.Set(ctx, sealedKeyName("beta"), sealed, 0).Err())
	_, err = newKeys(t, rdb, kek).ForSchema(ctx, "beta")
	require.Error(t, err, "a record moved to another store must not open")

	after, err := rdb.Get(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.Equal(t, sealed, after)
	again, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	assert.Equal(t, alpha.KID, again.KID)
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

	key, err := newKeys(t, rdb, testKEK(t)).ForSchema(ctx, "alpha")
	require.NoError(t, err)

	seed, err := rdb.Get(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.NotEqual(t, "stale-global-seed", seed,
		"a leftover global key must never become a schema's identity")
	assert.NotEmpty(t, key.KID)
}

// Sealing keeps every store's key: the sealed record holds the scalar
// the unsealed one held, so the key id and the published key are the
// ones platforms already verify against. The unsealed record stays
// until the deleting run, which removes it.
func TestSealUnsealedKeepsEveryStoresKey(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	alphaPlain := unsealedScalar(t, rdb, "alpha")
	betaPlain := unsealedScalar(t, rdb, "beta")
	// A store this release already serves from a sealed record.
	gamma, err := newKeys(t, rdb, kek).ForSchema(ctx, "gamma")
	require.NoError(t, err)

	keys := newKeys(t, rdb, kek)
	report, err := keys.SealUnsealed(ctx, false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, report.Sealed)
	assert.Empty(t, report.Conflicts)
	assert.Empty(t, report.Deleted)
	for schema, plain := range map[string]string{
		"alpha": alphaPlain, "beta": betaPlain,
	} {
		key, err := keys.ForSchema(ctx, schema)
		require.NoError(t, err)
		scalar, err := key.Private.Bytes()
		require.NoError(t, err)
		assert.Equal(t, plain, base64.StdEncoding.EncodeToString(scalar),
			"%s keeps its key", schema)
		still, err := rdb.Get(ctx, unsealedKeyName(schema)).Result()
		require.NoError(t, err, "pods of the earlier release still read it")
		assert.Equal(t, plain, still)
	}
	again, err := keys.ForSchema(ctx, "gamma")
	require.NoError(t, err)
	assert.Equal(t, gamma.KID, again.KID)

	// Run again, deleting: nothing is sealed twice, and every unsealed
	// record whose key is sealed is gone.
	report, err = newKeys(t, rdb, kek).SealUnsealed(ctx, true)
	require.NoError(t, err)
	assert.Empty(t, report.Sealed)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, report.AlreadySealed)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, report.Deleted)
	left, err := rdb.Keys(ctx, "ag:*:ucp:signing_key:es256").Result()
	require.NoError(t, err)
	assert.Empty(t, left)
	alpha, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	scalar, err := alpha.Private.Bytes()
	require.NoError(t, err)
	assert.Equal(t, alphaPlain, base64.StdEncoding.EncodeToString(scalar))
}

// A store whose unsealed record holds one key and sealed record another
// keeps both, reported: deleting either would rotate a key a platform
// may hold. So does a record that is not a key.
func TestSealUnsealedLeavesConflictsAlone(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	_, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	unsealedScalar(t, rdb, "alpha")
	require.NoError(t, rdb.Set(ctx, unsealedKeyName("beta"), "not a key",
		0).Err())

	report, err := newKeys(t, rdb, kek).SealUnsealed(ctx, true)
	require.NoError(t, err)
	assert.Contains(t, report.Conflicts, "alpha")
	assert.Contains(t, report.Conflicts, "beta")
	assert.Empty(t, report.Deleted)
	n, err := rdb.Exists(ctx, unsealedKeyName("alpha"),
		unsealedKeyName("beta")).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
}

// A store that has only an unsealed record is not given a new key: that
// would publish another key than the one platforms hold. It is refused
// until the record is sealed, and then served with its own key.
func TestNoKeyIsMintedOverAnUnsealedOne(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	plain := unsealedScalar(t, rdb, "alpha")

	_, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.ErrorIs(t, err, ucp.ErrKeyUnsealed)
	n, err := rdb.Exists(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.Zero(t, n)

	_, err = newKeys(t, rdb, kek).SealUnsealed(ctx, false)
	require.NoError(t, err)
	key, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	scalar, err := key.Private.Bytes()
	require.NoError(t, err)
	assert.Equal(t, plain, base64.StdEncoding.EncodeToString(scalar))
}

func TestProfilePublishesOnlyOwnKey(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	keys := newKeys(t, rdb, testKEK(t))

	alpha, err := keys.ForSchema(ctx, "alpha")
	require.NoError(t, err)
	beta, err := keys.ForSchema(ctx, "beta")
	require.NoError(t, err)

	serve := func(schema string) []map[string]string {
		tn := &tenant.Tenant{
			SchemaName:      schema,
			DefaultLocale:   "el",
			DefaultCurrency: "EUR",
			Domain:          schema + ".example.test",
		}
		req := httptest.NewRequest(http.MethodGet, "/.well-known/ucp", nil)
		req = req.WithContext(tenant.NewContext(req.Context(), tn))
		rec := httptest.NewRecorder()
		ucp.ProfileHandler(keys, config.HandlerEnvSandbox, quietLogger()).
			ServeHTTP(rec, req)
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

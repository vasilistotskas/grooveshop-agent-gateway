//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/ucp"
)

// A fresh environment, with no signing key stored, passes the boot
// check: there is nothing to check the key-encryption key against.
func TestCheckKEKPassesAnEnvironmentWithNoKeys(t *testing.T) {
	rdb := startRedis(t)
	require.NoError(t, newKeys(t, rdb, testKEK(t)).
		CheckKEK(context.Background(), quietLogger()))
}

// A key-encryption key that opens none of the stored keys (another
// environment's) refuses the boot; the right one passes.
func TestCheckKEKRefusesAKeyThatOpensNone(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	for _, schema := range []string{"alpha", "beta"} {
		_, err := newKeys(t, rdb, kek).ForSchema(ctx, schema)
		require.NoError(t, err)
	}

	err := newKeys(t, rdb, testKEK(t)).CheckKEK(ctx, quietLogger())
	require.ErrorIs(t, err, ucp.ErrKEKOpensNoKey)
	require.NoError(t, newKeys(t, rdb, kek).CheckKEK(ctx, quietLogger()))
}

// One record that does not open (copied from another store) beside keys
// that do is that store's fault, not the key-encryption key's: the boot
// goes on.
func TestCheckKEKPassesWhenAKeyOpens(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	_, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	sealed, err := rdb.Get(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	require.NoError(t, rdb.Set(ctx, sealedKeyName("beta"), sealed, 0).Err())

	require.NoError(t, newKeys(t, rdb, kek).CheckKEK(ctx, quietLogger()))
}

// The check ends with its context: a Redis that does not answer fails
// the boot instead of holding it.
func TestCheckKEKIsBoundByItsContext(t *testing.T) {
	rdb := startRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, newKeys(t, rdb, testKEK(t)).CheckKEK(ctx, quietLogger()))
}

// A record evicted while a pod holds the key is written back from the
// key the pod holds: an eviction never becomes a new key.
func TestRefreshWritesBackAnEvictedKey(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	pod := newKeys(t, rdb, kek)
	held, err := pod.ForSchema(ctx, "alpha")
	require.NoError(t, err)

	require.NoError(t, rdb.Del(ctx, sealedKeyName("alpha")).Err())
	pod.Refresh(ctx, quietLogger())

	again, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	assert.Equal(t, held.KID, again.KID, "the same key is back")
}

// A record holding another key than the pod's (another pod minted one
// after an eviction) is adopted, so every pod signs with the stored key.
func TestRefreshAdoptsTheStoredKey(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	pod := newKeys(t, rdb, kek)
	held, err := pod.ForSchema(ctx, "alpha")
	require.NoError(t, err)

	require.NoError(t, rdb.Del(ctx, sealedKeyName("alpha")).Err())
	minted, err := newKeys(t, rdb, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	require.NotEqual(t, held.KID, minted.KID)

	pod.Refresh(ctx, quietLogger())
	now, err := pod.ForSchema(ctx, "alpha")
	require.NoError(t, err)
	assert.Equal(t, minted.KID, now.KID, "the pod adopts the stored key")
}

// The refresh reads each held record, which keeps it recently used for
// the shared Redis's LRU eviction.
func TestRefreshKeepsTheRecordRecent(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	pod := newKeys(t, rdb, testKEK(t))
	_, err := pod.ForSchema(ctx, "alpha")
	require.NoError(t, err)

	time.Sleep(2500 * time.Millisecond)
	idle, err := rdb.ObjectIdleTime(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	require.GreaterOrEqual(t, idle, 2*time.Second)
	pod.Refresh(ctx, quietLogger())
	idle, err = rdb.ObjectIdleTime(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.Less(t, idle, 2*time.Second)
}

// The sentinel a mint leaves expires by itself, so stores first keyed
// after the seal command ran leave nothing behind for good; the key
// itself never expires.
func TestTheSentinelExpires(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	_, err := newKeys(t, rdb, testKEK(t)).ForSchema(ctx, "alpha")
	require.NoError(t, err)
	ttl, err := rdb.PTTL(ctx, unsealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.Positive(t, ttl)
	assert.LessOrEqual(t, ttl, 7*24*time.Hour)
	keyTTL, err := rdb.PTTL(ctx, sealedKeyName("alpha")).Result()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), keyTTL)
}

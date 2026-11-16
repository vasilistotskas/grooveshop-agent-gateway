//go:build integration

package integration

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
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

// proxyTo serves addr by forwarding each connection to target, until the
// test ends: a Redis that becomes reachable at addr once it starts.
func proxyTo(t *testing.T, addr, target string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				_ = in.Close()
				continue
			}
			go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
			go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
		}
	}()
}

// A pod that boots while Redis does not answer waits for it, as its
// readiness does, and is not ready until the check passed: it never
// gives up, so it never crash-loops into the restart backoff. Once
// Redis answers, the check passes and the pod is ready.
func TestAwaitKEKWaitsForRedisAndThenPasses(t *testing.T) {
	upstream := startRedis(t)
	ctx := context.Background()
	kek := testKEK(t)
	_, err := newKeys(t, upstream, kek).ForSchema(ctx, "alpha")
	require.NoError(t, err)

	// An address nothing listens on yet.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	keys := newKeys(t, rdb, kek)

	awaitCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- keys.AwaitKEK(awaitCtx, quietLogger()) }()

	time.Sleep(1500 * time.Millisecond)
	require.Error(t, keys.Checked(), "not ready while Redis does not answer")
	select {
	case err := <-done:
		t.Fatalf("the check gave up while Redis was down: %v", err)
	default:
	}

	proxyTo(t, addr, upstream.Options().Addr)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("the check did not pass once Redis answered")
	}
	require.NoError(t, keys.Checked(), "ready once the check passed")
}

// The definite failure (stored keys, none of which opens) ends the wait
// at once, and the pod never becomes ready: the process exits on it.
func TestAwaitKEKEndsOnAKeyThatOpensNone(t *testing.T) {
	rdb := startRedis(t)
	ctx := context.Background()
	_, err := newKeys(t, rdb, testKEK(t)).ForSchema(ctx, "alpha")
	require.NoError(t, err)

	keys := newKeys(t, rdb, testKEK(t))
	require.ErrorIs(t, keys.AwaitKEK(ctx, quietLogger()),
		ucp.ErrKEKOpensNoKey)
	require.Error(t, keys.Checked())
}

// The wait ends with its context: a shutdown stops it.
func TestAwaitKEKStopsWithItsContext(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1",
		MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	keys := newKeys(t, rdb, testKEK(t))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.ErrorIs(t, keys.AwaitKEK(ctx, quietLogger()),
		context.DeadlineExceeded)
	require.Error(t, keys.Checked())
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
